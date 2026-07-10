package gitea

import (
	"errors"
	"testing"
)

func TestClassifyPushFailure(t *testing.T) {
	genericErr := errors.New("exit status 1")

	cases := []struct {
		name string
		out  string
		want any // *NotFastForwardError, *ProtectedBranchError, or nil for generic
	}{
		{
			name: "non-fast-forward",
			out: "To https://gitea.example.com/o/r.git\n" +
				"!\trefs/heads/main:refs/heads/main\t[rejected] (non-fast-forward)\n" +
				"Done\n",
			want: &NotFastForwardError{},
		},
		{
			name: "fetch first",
			out: "To https://gitea.example.com/o/r.git\n" +
				"!\tabc123:refs/heads/main\t[rejected] (fetch first)\n" +
				"Done\n",
			want: &NotFastForwardError{},
		},
		{
			name: "remote rejected by branch protection",
			out: "To https://gitea.example.com/o/r.git\n" +
				"!\tabc123:refs/heads/main\t[remote rejected] (Not allowed to push to protected branch main.)\n" +
				"Done\n",
			want: &ProtectedBranchError{},
		},
		{
			name: "no status line",
			out:  "fatal: unable to access repo: connection refused\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyPushFailure("main", "abc123", tc.out, genericErr)
			switch tc.want.(type) {
			case *NotFastForwardError:
				var e *NotFastForwardError
				if !errors.As(err, &e) {
					t.Fatalf("got %v, want NotFastForwardError", err)
				}
			case *ProtectedBranchError:
				var e *ProtectedBranchError
				if !errors.As(err, &e) {
					t.Fatalf("got %v, want ProtectedBranchError", err)
				}
				if e.Message == "" {
					t.Fatal("expected server message in ProtectedBranchError")
				}
			default:
				if !errors.Is(err, genericErr) {
					t.Fatalf("got %v, want wrapped generic error", err)
				}
			}
		})
	}
}
