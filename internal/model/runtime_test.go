package model

import (
	"encoding/json"
	"testing"

	"github.com/drn/argus/internal/testutil"
)

func TestRuntime_String(t *testing.T) {
	cases := []struct {
		runtime Runtime
		want    string
	}{
		{RuntimeLocal, "local"},
		{RuntimeExeDev, "exedev"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			testutil.Equal(t, tc.runtime.String(), tc.want)
		})
	}
}

func TestRuntime_ParseRuntime(t *testing.T) {
	t.Run("empty defaults to local", func(t *testing.T) {
		r, err := ParseRuntime("")
		testutil.NoError(t, err)
		testutil.Equal(t, r, RuntimeLocal)
	})
	t.Run("local", func(t *testing.T) {
		r, err := ParseRuntime("local")
		testutil.NoError(t, err)
		testutil.Equal(t, r, RuntimeLocal)
	})
	t.Run("exedev", func(t *testing.T) {
		r, err := ParseRuntime("exedev")
		testutil.NoError(t, err)
		testutil.Equal(t, r, RuntimeExeDev)
	})
	t.Run("unknown errors", func(t *testing.T) {
		_, err := ParseRuntime("aws-fargate")
		if err == nil {
			t.Fatal("expected error for unknown runtime")
		}
	})
}

func TestRuntime_JSONRoundTrip(t *testing.T) {
	for _, want := range []Runtime{RuntimeLocal, RuntimeExeDev} {
		t.Run(want.String(), func(t *testing.T) {
			data, err := json.Marshal(want)
			testutil.NoError(t, err)
			var got Runtime
			testutil.NoError(t, json.Unmarshal(data, &got))
			testutil.Equal(t, got, want)
		})
	}
}
