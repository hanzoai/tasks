package nexus

import (
	"github.com/nexus-rpc/sdk-go/nexus"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	nexuspb "go.temporal.io/api/nexus/v1"
	matchingservice "go.temporal.io/server/api/matchingservice/v1"
	"go.temporal.io/server/common/nexus/nexusrpc"
)

// StartOperationResponseToError converts a StartOperationResponse proto into a Nexus SDK error.
// Returns nil if the response indicates success (SyncSuccess or AsyncSuccess).
//
// Error types returned:
//   - *nexus.HandlerError: internal errors (e.g., unexpected response variant)
//   - *nexus.OperationError: operation-level failures from the handler
func StartOperationResponseToError(resp *nexuspb.StartOperationResponse) error {
	switch t := resp.GetVariant().(type) {
	case *nexuspb.StartOperationResponse_SyncSuccess:
		return nil
	case *nexuspb.StartOperationResponse_AsyncSuccess:
		return nil
	case *nexuspb.StartOperationResponse_OperationError:
		//nolint:staticcheck // Deprecated variant still in use for backward compatibility.
		opErr := &nexus.OperationError{
			Message: "operation error",
			//nolint:staticcheck // Deprecated function still in use for backward compatibility.
			State: nexus.OperationState(t.OperationError.GetOperationState()),
			Cause: &nexus.FailureError{
				//nolint:staticcheck // Deprecated function still in use for backward compatibility.
				Failure: ProtoFailureToNexusFailure(t.OperationError.GetFailure()),
			},
		}
		if err := nexusrpc.MarkAsWrapperError(nexusrpc.DefaultFailureConverter(), opErr); err != nil {
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
		}
		return opErr
	case *nexuspb.StartOperationResponse_Failure:
		return operationErrorFromTemporalFailure(t.Failure)
	default:
		return nil
	}
}

// DispatchNexusTaskResponseToError converts a DispatchNexusTaskResponse proto into a Nexus SDK
// error. Returns nil if the response indicates success.
//
// This handles the outer dispatch envelope (timeout, handler error, failure) and delegates to
// StartOperationResponseToError for the inner StartOperationResponse.
//
// Error types returned:
//   - *nexus.HandlerError: transport/handler failures, timeouts
//   - *nexus.OperationError: operation-level failures from the worker
func DispatchNexusTaskResponseToError(resp *matchingservice.DispatchNexusTaskResponse) error {
	switch t := resp.GetOutcome().(type) {
	case *matchingservice.DispatchNexusTaskResponse_Failure:
		return handlerErrorFromTemporalFailure(t.Failure)
	case *matchingservice.DispatchNexusTaskResponse_HandlerError:
		return handlerErrorFromDeprecatedProto(t.HandlerError)
	case *matchingservice.DispatchNexusTaskResponse_RequestTimeout:
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeUpstreamTimeout, "upstream timeout")
	case *matchingservice.DispatchNexusTaskResponse_Response:
		return StartOperationResponseToError(t.Response.GetStartOperation())
	default:
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "empty outcome")
	}
}

func handlerErrorFromTemporalFailure(failure *failurepb.Failure) error {
	nf, err := TemporalFailureToNexusFailure(failure)
	if err != nil {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
	}
	he, err := nexusrpc.DefaultFailureConverter().FailureToError(nf)
	if err != nil {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
	}
	return he
}

func handlerErrorFromDeprecatedProto(he *nexuspb.HandlerError) *nexus.HandlerError {
	var retryBehavior nexus.HandlerErrorRetryBehavior
	//nolint:exhaustive // unspecified is the default
	switch he.GetRetryBehavior() {
	case enumspb.NEXUS_HANDLER_ERROR_RETRY_BEHAVIOR_RETRYABLE:
		retryBehavior = nexus.HandlerErrorRetryBehaviorRetryable
	case enumspb.NEXUS_HANDLER_ERROR_RETRY_BEHAVIOR_NON_RETRYABLE:
		retryBehavior = nexus.HandlerErrorRetryBehaviorNonRetryable
	}
	//nolint:staticcheck // Deprecated function still in use for backward compatibility.
	cause := ProtoFailureToNexusFailure(he.GetFailure())
	return &nexus.HandlerError{
		//nolint:staticcheck // Deprecated function still in use for backward compatibility.
		Type:          nexus.HandlerErrorType(he.GetErrorType()),
		RetryBehavior: retryBehavior,
		Cause:         &nexus.FailureError{Failure: cause},
	}
}

func operationErrorFromTemporalFailure(failure *failurepb.Failure) error {
	state := nexus.OperationStateFailed
	if failure.GetCanceledFailureInfo() != nil {
		state = nexus.OperationStateCanceled
	}
	nf, err := TemporalFailureToNexusFailure(failure)
	if err != nil {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
	}
	cause, err := nexusrpc.DefaultFailureConverter().FailureToError(nf)
	if err != nil {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
	}
	opErr := &nexus.OperationError{
		State:   state,
		Message: "operation error",
		Cause:   cause,
	}
	if err := nexusrpc.MarkAsWrapperError(nexusrpc.DefaultFailureConverter(), opErr); err != nil {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "internal error")
	}
	return opErr
}
