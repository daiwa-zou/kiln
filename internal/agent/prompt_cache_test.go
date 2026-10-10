package agent

import (
	"context"
	"testing"
)

// Generate resumes the analyze session and forks it, so each attempt starts from
// the analysis alone. A plain resume would append attempt 0's transcript to the
// session attempt 1 resumes.
func TestBuildArgsGenerateForksTheAnalyzeSession(t *testing.T) {
	gen := BuildArgs(Request{
		Step: StepGenerate, WorkDir: "/src", ScratchDir: "/scratch",
		SessionID: "abc", Prompt: "write",
	})
	if got := flagVal(gen, "--resume"); got != SessionUUID("abc") {
		t.Errorf("--resume = %q, want the analyze session %q", got, SessionUUID("abc"))
	}
	if !containsArg(gen, "--fork-session") {
		t.Errorf("generate resumes without forking: %v", gen)
	}
	if containsArg(gen, "--session-id") {
		t.Errorf("generate must not name a session of its own: %v", gen)
	}
	// Forking is meaningless, and rejected, without a session to fork.
	noSession := BuildArgs(Request{
		Step: StepGenerate, WorkDir: "/src", ScratchDir: "/scratch", Prompt: "write",
	})
	if containsArg(noSession, "--fork-session") || containsArg(noSession, "--resume") {
		t.Errorf("a request with no session must not resume or fork: %v", noSession)
	}

	analyze := BuildArgs(Request{Step: StepAnalyze, WorkDir: "/src", SessionID: "abc", Prompt: "go"})
	if containsArg(analyze, "--fork-session") {
		t.Errorf("analyze is the session the others fork from: %v", analyze)
	}
}

// The CLI runner has its own session; the unit's material reaches it through the
// analyze prompt, and CacheableUnit must not be silently appended anywhere.
func TestBuildArgsIgnoresCacheableUnit(t *testing.T) {
	const unit = "UNIT-SOURCE-MARKER"
	for _, step := range []Step{StepAnalyze, StepGenerate} {
		args := BuildArgs(Request{
			Step: step, WorkDir: "/src", ScratchDir: "/scratch", SessionID: "abc",
			Prompt: "p", CacheableUnit: unit,
		})
		for _, a := range args {
			if a == unit {
				t.Errorf("%s: CacheableUnit leaked into argv", step)
			}
		}
	}
}

// CachesUnit is an opt-in: a runner that does not say so must be treated as one
// that reads the material from Prompt.
func TestCachesUnitIsOptIn(t *testing.T) {
	if !CachesUnit(&APIRunner{}) || !CachesUnit(NewFakeRunner(FakeOptions{})) {
		t.Error("the API-shaped runners must report that they take the unit as a cacheable block")
	}
	if CachesUnit(&ClaudeRunner{}) {
		t.Error("the CLI runner resumes a session and takes the material in the analyze prompt")
	}
	if CachesUnit(&stubRunner{}) {
		t.Error("a runner that never opted in must not be sent the material in a field it ignores")
	}
}

// The fake finds the source in CacheableUnit, not Prompt, so a changed source
// file has to change the page or the dev loop would stop noticing edits.
func TestFakeFingerprintCoversTheCacheableUnit(t *testing.T) {
	f := NewFakeRunner(FakeOptions{})
	req := fakeGenReq(t, "module:x", nil)
	req.CacheableUnit = "source v1"

	a, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.CacheableUnit = "source v2"
	b, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Generation.Pages[0].Body == b.Generation.Pages[0].Body {
		t.Error("a changed unit block did not change the page fingerprint")
	}
}
