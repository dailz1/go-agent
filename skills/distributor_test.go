package skills

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func loadBasic(t *testing.T) *Set {
	t.Helper()
	set, err := Load("testdata/basic")
	if err != nil {
		t.Fatalf("Load(testdata/basic) error = %v", err)
	}
	return set
}

func TestDistributorInfo(t *testing.T) {
	info := loadBasic(t).Tool().Info()
	if info.Name != "skill" || info.RequiresApproval {
		t.Errorf("Info() name/approval = %q, %v", info.Name, info.RequiresApproval)
	}
	if info.Parameters.Type != "object" || !reflect.DeepEqual(info.Parameters.Required, []string{"name"}) ||
		info.Parameters.Properties["name"].Type != "string" {
		t.Errorf("Info() parameters = %#v", info.Parameters)
	}
	if !reflect.DeepEqual(info.Description, wantBasicCatalog) {
		t.Errorf("Info() description is not the catalog")
	}
}

func TestDistributorReturnsBodyVerbatim(t *testing.T) {
	result, err := loadBasic(t).Tool().Execute(t.Context(), []byte(`{"name":"git-release"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError() || result.Data != nil {
		t.Fatalf("Execute() = %#v, want a plain hit with nil Data", result)
	}
	want := "# Git release checklist\n\n1. Run the full test suite.\n2. Tag `vX.Y.Z`.\n\n   Push the tag with `git push origin vX.Y.Z`."
	if result.Content != want {
		t.Errorf("Content =\n%q\nwant\n%q", result.Content, want)
	}
	if result.Status != tool.ResultSuccess {
		t.Errorf("Status = %q, want empty success", result.Status)
	}
}

func TestDistributorUnknownNameIsSoft(t *testing.T) {
	result, err := loadBasic(t).Tool().Execute(t.Context(), []byte(`{"name":"nope"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError() || result.Content != `skill "nope" not found` {
		t.Fatalf("Execute() = %#v, want soft skill-not-found", result)
	}
}

func TestDistributorMalformedArgumentsAreSoft(t *testing.T) {
	dist := loadBasic(t).Tool()
	for _, args := range []json.RawMessage{nil, []byte("null"), []byte("[]"), []byte(`"git-release"`), []byte(`{}`), []byte(`{"name":null}`), []byte(`{"name":1}`), []byte("{")} {
		result, err := dist.Execute(t.Context(), args)
		if err != nil || result == nil || !result.IsError() {
			t.Fatalf("Execute(%s) = %#v, %v; want a soft error", args, result, err)
		}
	}
}

func TestDistributorIgnoresExtraFields(t *testing.T) {
	result, err := loadBasic(t).Tool().Execute(t.Context(), []byte(`{"name":"pdf-forms","extra":true}`))
	if err != nil || result.IsError() {
		t.Fatalf("Execute() = %#v, %v; want the pdf-forms hit", result, err)
	}
	if result.Content != "Fill form fields with `pdfcpu fill in.pdf fields.json out.pdf`, then flatten." {
		t.Errorf("Content = %q", result.Content)
	}
}

func TestDistributorPassesRegistryValidation(t *testing.T) {
	set := loadBasic(t)
	registry := tool.NewRegistry()
	if err := registry.Register(set.Tool()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, ok := registry.Get("skill"); !ok {
		t.Fatal("registered tool not found under its name")
	}
}

func TestToolRepeatedCallsHaveIdenticalInfo(t *testing.T) {
	set := loadBasic(t)
	if !reflect.DeepEqual(set.Tool().Info(), set.Tool().Info()) {
		t.Error("Tool().Info() differs across calls")
	}
}

func TestSkillsAndIssuesReturnCopies(t *testing.T) {
	set, err := Load("testdata/missing-file")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	skills := set.Skills()
	skills[0].Name = "mutated"
	if set.Skills()[0].Name == "mutated" {
		t.Error("mutating Skills() result leaked into the Set")
	}
	issues := set.Issues()
	issues[0].Problem = "mutated"
	if set.Issues()[0].Problem == "mutated" {
		t.Error("mutating Issues() result leaked into the Set")
	}
}

func TestDistributorConcurrentExecute(t *testing.T) {
	dist := loadBasic(t).Tool()
	calls := []struct {
		args    string
		wantHit bool
	}{
		{`{"name":"git-release"}`, true},
		{`{"name":"pdf-forms"}`, true},
		{`{"name":"missing"}`, false},
		{`not json`, false},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call := calls[i%len(calls)]
			for j := 0; j < 25; j++ {
				result, err := dist.Execute(context.Background(), []byte(call.args))
				if err != nil {
					t.Errorf("Execute(%s) error = %v", call.args, err)
					return
				}
				if result == nil || result.IsError() == call.wantHit {
					t.Errorf("Execute(%s) = %#v, wantHit = %v", call.args, result, call.wantHit)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
