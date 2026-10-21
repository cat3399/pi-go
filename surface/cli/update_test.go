package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/selfupdate"
)

type updateStub struct {
	options selfupdate.Options
	plan    selfupdate.Plan
	err     error
	applied bool
}

func (s *updateStub) Check(_ context.Context, options selfupdate.Options) (selfupdate.Plan, error) {
	s.options = options
	return s.plan, s.err
}

func (s *updateStub) Apply(_ context.Context, plan selfupdate.Plan) (selfupdate.Result, error) {
	s.applied = true
	return selfupdate.Result{Plan: plan, Updated: plan.Available, Backup: "/installation/previous-pi-go"}, s.err
}

func TestUpdateHelpAndInvalidArgumentsDoNotInitializeUpdater(t *testing.T) {
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"help"}, 0},
		{[]string{"--latest-version"}, 2},
		{[]string{"--surface", "web"}, 2},
		{[]string{"--version"}, 2},
		{[]string{"--version", "1.2"}, 2},
		{[]string{"--version", "1.2.3-rc.1"}, 2},
		{[]string{"unexpected"}, 2},
	} {
		var stdout, stderr bytes.Buffer
		code := runUpdate(context.Background(), test.args, &stdout, &stderr, func() updateService {
			t.Fatalf("updater initialized for %v", test.args)
			return nil
		})
		if code != test.code {
			t.Errorf("%v: code = %d, want %d; stderr = %s", test.args, code, test.code, stderr.String())
		}
	}
}

func TestUpdateCheckReportsLatestWithoutInstalling(t *testing.T) {
	stub := &updateStub{plan: selfupdate.Plan{CurrentVersion: "0.1.0", TargetVersion: "0.2.0", Surface: "web", OS: "linux", Arch: "arm64", Executable: "/installation/pi-go", Action: "upgrade", Available: true, CanUpdate: true}}
	var stdout, stderr bytes.Buffer
	if code := runUpdate(context.Background(), []string{"--check", "--json"}, &stdout, &stderr, func() updateService { return stub }); code != 0 {
		t.Fatalf("code = %d; stderr = %s", code, stderr.String())
	}
	var plan selfupdate.Plan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatalf("stdout is not clean JSON: %s (%v)", stdout.String(), err)
	}
	if plan.TargetVersion != "0.2.0" || plan.Surface != "web" || !plan.Available || stub.applied {
		t.Fatalf("check: %+v, applied = %v", plan, stub.applied)
	}
	if !strings.Contains(stderr.String(), "Checking") {
		t.Fatal("progress not written to stderr")
	}
}

func TestUpdateAppliesResolvedPlanAndPropagatesOptions(t *testing.T) {
	stub := &updateStub{plan: selfupdate.Plan{CurrentVersion: "0.1.0", TargetVersion: "0.1.0", Surface: "terminal", Action: "reinstall", Available: true, CanUpdate: true}}
	var stdout, stderr bytes.Buffer
	code := runUpdate(context.Background(), []string{"--self", "--force", "--version", "v0.1.0", "--json"}, &stdout, &stderr, func() updateService { return stub })
	var result selfupdate.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !stub.applied || !stub.options.Force || stub.options.Version != "v0.1.0" || !result.Updated || result.Backup == "" {
		t.Fatalf("code = %d; result = %+v; options = %+v; stderr = %s", code, result, stub.options, stderr.String())
	}
}

func TestUpdateFailureAndUnsupportedInstallation(t *testing.T) {
	for _, stub := range []*updateStub{
		{err: errors.New("network unavailable")},
		{plan: selfupdate.Plan{CanUpdate: false, Reason: "use the original installer"}},
	} {
		var stdout, stderr bytes.Buffer
		code := runUpdate(context.Background(), nil, &stdout, &stderr, func() updateService { return stub })
		if code != 1 || stub.applied || !strings.Contains(stderr.String(), "pi-go update:") {
			t.Fatalf("code = %d, applied = %v; stderr = %s", code, stub.applied, stderr.String())
		}
	}
}
