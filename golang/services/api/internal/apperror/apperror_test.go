package apperror

import (
	"errors"
	"net/http"
	"testing"
)

func TestNew(t *testing.T) {
	ae := New(CodeInternal, "something broke", http.StatusInternalServerError)
	if ae.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", ae.Code, CodeInternal)
	}
	if ae.Message != "something broke" {
		t.Errorf("Message = %q", ae.Message)
	}
	if ae.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d", ae.HTTPStatus)
	}
}

func TestWrap(t *testing.T) {
	cause := errors.New("db down")
	ae := Wrap(CodeInternal, "internal error", http.StatusInternalServerError, cause)
	if ae.Detail != "db down" {
		t.Errorf("Detail = %q, want %q", ae.Detail, "db down")
	}
}

func TestWrap_NilCause(t *testing.T) {
	ae := Wrap(CodeInternal, "msg", http.StatusInternalServerError, nil)
	if ae.Detail != "" {
		t.Errorf("Detail should be empty for nil cause, got %q", ae.Detail)
	}
}

func TestError_String(t *testing.T) {
	ae := New(CodeTimeout, "timed out", http.StatusGatewayTimeout)
	got := ae.Error()
	want := "TIMEOUT: timed out"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAs(t *testing.T) {
	ae := New(CodeInternal, "msg", http.StatusInternalServerError)
	wrapped := errors.New("outer: " + ae.Error())
	// As should work when the error IS an *AppError
	got, ok := As(ae)
	if !ok || got != ae {
		t.Error("As should unwrap *AppError directly")
	}
	// As should return false for a plain error
	_, ok = As(wrapped)
	if ok {
		t.Error("As should return false for non-AppError")
	}
}

func TestClassify_AlreadyClassified(t *testing.T) {
	ae := New(CodeTaskFailed, "task failed", http.StatusBadGateway)
	got := Classify(ae)
	if got != ae {
		t.Error("Classify should return the same *AppError when already classified")
	}
}

func TestClassify_Nil(t *testing.T) {
	got := Classify(nil)
	if got != nil {
		t.Errorf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassify_Timeout(t *testing.T) {
	ae := Classify(errors.New("context deadline exceeded"))
	if ae.Code != CodeTimeout {
		t.Errorf("Code = %q, want %q", ae.Code, CodeTimeout)
	}
	if ae.HTTPStatus != http.StatusGatewayTimeout {
		t.Errorf("HTTPStatus = %d, want %d", ae.HTTPStatus, http.StatusGatewayTimeout)
	}
}

func TestClassify_AgentNotFound(t *testing.T) {
	ae := Classify(errors.New(`agent "foo": agent not found`))
	if ae.Code != CodeAgentNotFound {
		t.Errorf("Code = %q, want %q", ae.Code, CodeAgentNotFound)
	}
}

func TestClassify_PlannerFailed(t *testing.T) {
	ae := Classify(errors.New("planner failed (gen=0): connection refused"))
	if ae.Code != CodePlannerFailed {
		t.Errorf("Code = %q, want %q", ae.Code, CodePlannerFailed)
	}
}

func TestClassify_TaskFailed(t *testing.T) {
	ae := Classify(errors.New("all tasks failed: rpc error"))
	if ae.Code != CodeTaskFailed {
		t.Errorf("Code = %q, want %q", ae.Code, CodeTaskFailed)
	}
}

func TestClassify_MaxIterations(t *testing.T) {
	ae := Classify(errors.New("exceeded max_iterations"))
	if ae.Code != CodeMaxIterationsReached {
		t.Errorf("Code = %q, want %q", ae.Code, CodeMaxIterationsReached)
	}
}

func TestClassify_DAGValidation(t *testing.T) {
	ae := Classify(errors.New("DAG validation failed: tool not in spec"))
	if ae.Code != CodeDAGValidationFailed {
		t.Errorf("Code = %q, want %q", ae.Code, CodeDAGValidationFailed)
	}
}

func TestClassify_HITL(t *testing.T) {
	ae := Classify(errors.New("human rejected the action (hitl)"))
	if ae.Code != CodeHITLRejected {
		t.Errorf("Code = %q, want %q", ae.Code, CodeHITLRejected)
	}
}

func TestClassify_Default(t *testing.T) {
	ae := Classify(errors.New("some weird unclassified error"))
	if ae.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", ae.Code, CodeInternal)
	}
	if ae.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d, want %d", ae.HTTPStatus, http.StatusInternalServerError)
	}
}
