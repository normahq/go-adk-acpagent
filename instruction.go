package acpagent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	adkagent "google.golang.org/adk/v2/agent"
)

// InstructionProvider allows ACP instructions to be created dynamically using
// invocation context, mirroring llmagent semantics.
type InstructionProvider func(ctx adkagent.ReadonlyContext) (string, error)

type resolvedInstructionParts struct {
	global      string
	instruction string
}

func (r resolvedInstructionParts) combined() string {
	instructions := make([]string, 0, 2)
	if strings.TrimSpace(r.global) != "" {
		instructions = append(instructions, strings.TrimSpace(r.global))
	}
	if strings.TrimSpace(r.instruction) != "" {
		instructions = append(instructions, strings.TrimSpace(r.instruction))
	}
	return strings.Join(instructions, "\n\n")
}

var placeholderRegex = regexp.MustCompile(`{+[^{}]*}+`)

const (
	appPrefix  = "app:"
	userPrefix = "user:"
	tempPrefix = "temp:"
)

func prependInstructionsToPrompt(instructions string, prompt string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return prompt
	}
	return instructions + "\n\nUser message:\n" + prompt
}

func normalizeInstruction(primary, deprecated string) string {
	inst := strings.TrimSpace(primary)
	if inst != "" {
		return inst
	}
	return strings.TrimSpace(deprecated)
}

func (a *Agent) resolveInstructionParts(ctx adkagent.InvocationContext) (resolvedInstructionParts, error) {
	readonlyCtx := readonlyInvocationContext{invocation: ctx}

	globalInstruction, err := a.resolveSingleInstruction(
		ctx,
		readonlyCtx,
		a.globalInstruction,
		a.globalInstructionProvider,
		"global instruction",
	)
	if err != nil {
		return resolvedInstructionParts{}, err
	}

	instruction, err := a.resolveSingleInstruction(
		ctx,
		readonlyCtx,
		a.instruction,
		a.instructionProvider,
		"instruction",
	)
	if err != nil {
		return resolvedInstructionParts{}, err
	}

	return resolvedInstructionParts{
		global:      strings.TrimSpace(globalInstruction),
		instruction: strings.TrimSpace(instruction),
	}, nil
}

func addInstructionMetaToSessionConfig(cfg acpSessionConfig, instructions resolvedInstructionParts) (acpSessionConfig, error) {
	if instructions.combined() == "" {
		return cfg, nil
	}

	if cfg.meta == nil {
		cfg.meta = map[string]any{}
	}
	codexMeta := map[string]any{}
	if rawCodexMeta, ok := cfg.meta["codex"]; ok {
		existingCodexMeta, ok := rawCodexMeta.(map[string]any)
		if !ok {
			return acpSessionConfig{}, fmt.Errorf("acp session meta codex must be an object; got %T", rawCodexMeta)
		}
		codexMeta = cloneAnyMap(existingCodexMeta)
	}

	setInstructionMetaIfEmpty(codexMeta, "baseInstructions", instructions.global)
	setInstructionMetaIfEmpty(codexMeta, "developerInstructions", instructions.instruction)
	if len(codexMeta) > 0 {
		cfg.meta["codex"] = codexMeta
	}

	metaJSON, err := json.Marshal(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
	}
	cfg.metaJSON = string(metaJSON)
	return cfg, nil
}

func addReasoningEffortToSessionConfig(cfg acpSessionConfig, reasoningEffort string) (acpSessionConfig, error) {
	trimmedEffort := strings.TrimSpace(reasoningEffort)
	if trimmedEffort == "" {
		return cfg, nil
	}
	if cfg.meta == nil {
		cfg.meta = map[string]any{}
	}

	codexMeta, err := codexMetaObject(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, err
	}
	configMeta, err := codexConfigObject(codexMeta)
	if err != nil {
		return acpSessionConfig{}, err
	}
	configMeta["model_reasoning_effort"] = trimmedEffort
	codexMeta["config"] = configMeta
	cfg.meta["codex"] = codexMeta

	metaJSON, err := json.Marshal(cfg.meta)
	if err != nil {
		return acpSessionConfig{}, fmt.Errorf("marshal acp session meta: %w", err)
	}
	cfg.metaJSON = string(metaJSON)
	return cfg, nil
}

func codexMetaObject(meta map[string]any) (map[string]any, error) {
	codexMeta := map[string]any{}
	if rawCodexMeta, ok := meta["codex"]; ok {
		existingCodexMeta, ok := rawCodexMeta.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("acp session meta codex must be an object; got %T", rawCodexMeta)
		}
		codexMeta = cloneAnyMap(existingCodexMeta)
	}
	return codexMeta, nil
}

func codexConfigObject(codexMeta map[string]any) (map[string]any, error) {
	configMeta := map[string]any{}
	if rawConfigMeta, ok := codexMeta["config"]; ok {
		existingConfigMeta, ok := rawConfigMeta.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("acp session meta codex.config must be an object; got %T", rawConfigMeta)
		}
		configMeta = cloneAnyMap(existingConfigMeta)
	}
	return configMeta, nil
}

func setInstructionMetaIfEmpty(meta map[string]any, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if existing, ok := meta[key]; ok && isNonEmptyMetaValue(existing) {
		return
	}
	meta[key] = value
}

func isNonEmptyMetaValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

func (a *Agent) resolveSingleInstruction(invocationCtx adkagent.InvocationContext, ctx adkagent.ReadonlyContext, templateInstruction string, provider InstructionProvider, kind string) (string, error) {
	if provider != nil {
		instruction, err := provider(ctx)
		if err != nil {
			return "", fmt.Errorf("evaluate %s provider: %w", kind, err)
		}
		return instruction, nil
	}

	templateInstruction = strings.TrimSpace(templateInstruction)
	if templateInstruction == "" {
		return "", nil
	}

	instruction, err := injectSessionState(invocationCtx, templateInstruction)
	if err != nil {
		return "", fmt.Errorf("inject session state into %s: %w", kind, err)
	}
	return instruction, nil
}

func injectSessionState(ctx adkagent.InvocationContext, templateInstruction string) (string, error) {
	var result strings.Builder
	lastIndex := 0
	matches := placeholderRegex.FindAllStringIndex(templateInstruction, -1)

	for _, matchIndexes := range matches {
		startIndex, endIndex := matchIndexes[0], matchIndexes[1]
		result.WriteString(templateInstruction[lastIndex:startIndex])

		replacement, err := replaceTemplateMatch(ctx, templateInstruction[startIndex:endIndex])
		if err != nil {
			return "", err
		}
		result.WriteString(replacement)

		lastIndex = endIndex
	}

	result.WriteString(templateInstruction[lastIndex:])
	return result.String(), nil
}

func replaceTemplateMatch(ctx adkagent.InvocationContext, match string) (string, error) {
	varName := strings.TrimSpace(strings.Trim(match, "{}"))
	optional := false
	if strings.HasSuffix(varName, "?") {
		optional = true
		varName = strings.TrimSuffix(varName, "?")
	}

	if after, ok := strings.CutPrefix(varName, "artifact."); ok {
		if ctx.Artifacts() == nil {
			return "", fmt.Errorf("artifact service is not initialized")
		}
		resp, err := ctx.Artifacts().Load(ctx, after)
		if err != nil {
			if optional {
				return "", nil
			}
			return "", fmt.Errorf("load artifact %q: %w", after, err)
		}
		return resp.Part.Text, nil
	}

	if !isValidStateName(varName) {
		return match, nil
	}

	value, err := ctx.Session().State().Get(varName)
	if err != nil {
		if optional {
			return "", nil
		}
		return "", err
	}
	if value == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", value), nil
}

func isValidStateName(varName string) bool {
	parts := strings.Split(varName, ":")
	if len(parts) == 1 {
		return isIdentifier(varName)
	}

	if len(parts) == 2 {
		prefix := parts[0] + ":"
		validPrefixes := []string{appPrefix, userPrefix, tempPrefix}
		if slices.Contains(validPrefixes, prefix) {
			return isIdentifier(parts[1])
		}
	}
	return false
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}
