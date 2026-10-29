package acpagent

import (
	"context"
	"errors"

	acp "github.com/coder/acp-go-sdk"
)

var (
	// ErrModelDescriptorInvalid means no single valid semantic Model descriptor
	// was available before mutation.
	ErrModelDescriptorInvalid = errors.New("acp model descriptor is unavailable")
	// ErrDesiredModelUnavailable means the desired model could not be observed.
	ErrDesiredModelUnavailable = errors.New("acp desired model verification unavailable")
	// ErrDesiredModelUncertain means a mutation may have happened without an
	// authoritative confirming response.
	ErrDesiredModelUncertain = errors.New("acp desired model outcome is uncertain")
	// ErrDesiredModelRejected means authoritative evidence did not accept the
	// requested model.
	ErrDesiredModelRejected = errors.New("acp desired model was rejected")
)

func boundedModelAdmissionError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, ErrSessionBusy):
		return ErrSessionBusy
	case errors.Is(err, ErrClientClosed):
		return ErrClientClosed
	case errors.Is(err, ErrModelRegistryFull):
		return ErrModelRegistryFull
	default:
		return errors.New("acp adk session admission failed")
	}
}

// applyDesiredModelHeld reconciles one authoritative descriptor snapshot while
// the exact ADK-session admission is held. A failed Set is never retried here.
func (a *Agent) applyDesiredModelHeld(
	ctx context.Context,
	admission *modelAdmission,
	remoteID string,
	options []acp.SessionConfigOption,
) (*sessionControlState, error) {
	attachment := admission.replaceAttachment(remoteID)
	return a.applyDesiredModelCurrentHeld(ctx, admission, remoteID, options, attachment)
}

func (a *Agent) applyDesiredModelCurrentHeld(
	ctx context.Context,
	admission *modelAdmission,
	remoteID string,
	options []acp.SessionConfigOption,
	attachment uint64,
) (*sessionControlState, error) {
	control, descriptorValid := classifyModelDescriptor(options, a.attachmentGeneration)
	admission.confirmCurrentAttachment(control, attachment)
	desired := admission.desired(a.desiredModel)
	if desired == "" {
		return control, nil
	}
	if !descriptorValid {
		admission.invalidateVerification()
		return nil, ErrModelDescriptorInvalid
	}
	if !control.hasModel(desired) {
		admission.invalidateVerification()
		return control, ErrDesiredModelRejected
	}
	if control.currentModel == desired {
		if !admission.confirm(desired, a.desiredModel, attachment) {
			return nil, ErrDesiredModelUnavailable
		}
		return control, nil
	}
	if err := ctx.Err(); err != nil {
		admission.invalidateVerification()
		return control, err
	}
	resp, err := a.client.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(remoteID),
			ConfigId:  acp.SessionConfigId(control.modelOptionID),
			Value:     acp.SessionConfigValueId(desired),
		},
	})
	if err != nil {
		admission.invalidateVerification()
		if isDefinitiveModelRejection(err) {
			return control, ErrDesiredModelRejected
		}
		return control, ErrDesiredModelUncertain
	}
	verified := newSessionControlState(resp.ConfigOptions, a.attachmentGeneration)
	if verified == nil {
		admission.invalidateVerification()
		return nil, ErrDesiredModelUncertain
	}
	if verified.currentModel != desired {
		admission.invalidateVerification()
		return verified, ErrDesiredModelRejected
	}
	if !admission.confirmCurrentAttachment(verified, attachment) || !admission.confirm(desired, a.desiredModel, attachment) {
		return nil, ErrDesiredModelUnavailable
	}
	return verified, nil
}

func isDefinitiveModelRejection(err error) bool {
	if isACPMethodNotFoundError(err) {
		return true
	}
	var requestErr *acp.RequestError
	return errors.As(err, &requestErr) && (requestErr.Code == -32600 || requestErr.Code == -32602)
}
