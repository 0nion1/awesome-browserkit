package main

import (
	"context"
	"testing"

	browserkit "github.com/zeoxisca/awesome-browserkit"
)

func TestInvokeActPreservesFailuresAndPartialResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result browserkit.ActResponse
		err    error
		ok     bool
	}{
		{"failed step", browserkit.ActResponse{Completed: 1, FailedStep: 2, Steps: []browserkit.ActStepResult{{Index: 0, Success: true}, {Index: 1, Error: "step failed"}}}, nil, false},
		{"canceled observation", browserkit.ActResponse{Completed: 1, Cancelled: true, ScreenshotError: context.Canceled.Error()}, context.Canceled, false},
		{"canceled receipt", browserkit.ActResponse{Completed: 1, Cancelled: true}, nil, false},
		{"failed observation", browserkit.ActResponse{Completed: 1, SnapshotError: "snapshot failed", ScreenshotError: "screenshot failed"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			functions := map[string]browserkit.Function{"browserAct": {
				Handler: func(context.Context, browserkit.ActRequest) (browserkit.ActResponse, error) { return tc.result, tc.err },
			}}
			response := invoke(context.Background(), functions, []byte(`{"function":"browserAct"}`))
			result, ok := response.Result.(browserkit.ActResponse)
			if !ok || response.OK != tc.ok || (response.Error != "") == tc.ok || result.Completed != tc.result.Completed || result.FailedStep != tc.result.FailedStep || result.Cancelled != tc.result.Cancelled || result.ScreenshotError != tc.result.ScreenshotError || result.SnapshotError != tc.result.SnapshotError {
				t.Fatalf("unexpected response: %+v", response)
			}
		})
	}
}
