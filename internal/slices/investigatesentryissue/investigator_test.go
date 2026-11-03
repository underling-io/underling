package investigatesentryissue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"underling/internal/events"
	"underling/internal/platform/eventstore"
)

// These tests specify the investigator's decisions, in memory: given the
// events already on the issue's stream, when an alert arrives and git,
// Claude and GitHub behave a certain way, then the alert adds exactly these
// events to the stream, and none to any other stream.

func TestAFixThatCanBePushedOpensADraftPR(t *testing.T) {
	s := given(t).
		when(anAlert()).
		then(started(), prOpened(testPRURL))

	alert := decodeAlert(t, anAlert())
	if want := investigationPrompt(alert, testBranch); s.infra.prompt != want {
		t.Errorf("claude got prompt %q, want %q", s.infra.prompt, want)
	}
	want := pullRequest{title: prTitle(alert), head: testBranch, base: "main", body: prBody(alert, testSummary)}
	if s.infra.pr == nil || *s.infra.pr != want {
		t.Errorf("opened PR %+v, want %+v", s.infra.pr, want)
	}
	s.andTheWorkspaceIsRemoved()
}

func TestNoCommitsMeanNoChanges(t *testing.T) {
	given(t).
		when(anAlert(), claudeCommitsNothing).
		then(started(), noChanges()).
		andNothingIsPushed().
		andTheWorkspaceIsRemoved()
}

func TestAStaleAlertIsSkipped(t *testing.T) {
	given(t).
		when(aStaleAlert()).
		then(skipped(events.InvestigationSkippedStale)).
		andNothingIsCloned()
}

func TestAnAlertForAnUnmappedProjectIsSkipped(t *testing.T) {
	given(t).
		when(anUnmappedAlert()).
		then(skipped(events.InvestigationSkippedUnmapped)).
		andNothingIsCloned().
		andLogged(map[string]any{"msg": "sentry issue investigation finished", "handler": handlerName, "issueId": testIssueID, "outcome": "skipped", "reason": "unmapped"})
}

func TestAnAlertBothStaleAndUnmappedIsSkippedAsStale(t *testing.T) {
	given(t).
		when(aStaleUnmappedAlert()).
		then(skipped(events.InvestigationSkippedStale)).
		andNothingIsCloned()
}

func TestAnAlertForAnIssueAlreadyInvestigatedAddsNothing(t *testing.T) {
	given(t, started(), noChanges()).
		when(anAlert()).
		then().
		andNothingIsCloned().
		andLogged(map[string]any{"msg": "sentry issue already handled", "outcome": "duplicate"})
}

func TestAnAlertForAnIssueAlreadySkippedAddsNothing(t *testing.T) {
	given(t, skipped(events.InvestigationSkippedUnmapped)).
		when(anUnmappedAlert()).
		then()
}

func TestAFailedCloneFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), cloneFails("git clone: exit status 128: repository not found")).
		then(started(), failed(events.InvestigationFailedClone, "git clone: exit status 128: repository not found"))
}

func TestAFailedClaudeRunFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), claudeFails("claude: exit status 1: model refused")).
		then(started(), failed(events.InvestigationFailedClaude, "claude: exit status 1: model refused")).
		andNothingIsPushed().
		andTheWorkspaceIsRemoved()
}

func TestCommitsOnOtherBranchesFailTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), claudeCommitsOn("claude-own", "claude-renamed")).
		then(started(), failed(events.InvestigationFailedClaude, "claude committed on branches other than "+testBranch+", which underling doesn't push: claude-own, claude-renamed")).
		andNothingIsPushed()
}

func TestClaudeRunningOutOfTimeFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), claudeTimesOut).
		then(started(), failed(events.InvestigationFailedTimeout, "claude did not finish within 30m0s")).
		andNothingIsPushed().
		andTheWorkspaceIsRemoved()
}

func TestFailingToListBranchesFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), listingBranchesFails("git for-each-ref: exit status 128")).
		then(started(), failed(events.InvestigationFailedClaude, "collect claude's commits: git for-each-ref: exit status 128")).
		andNothingIsPushed()
}

func TestFailingToCountCommitsFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), countingCommitsFails("git rev-list: exit status 128")).
		then(started(), failed(events.InvestigationFailedClaude, "collect claude's commits: git rev-list: exit status 128")).
		andNothingIsPushed()
}

func TestFailingToListChangedPathsFailsThePush(t *testing.T) {
	given(t).
		when(anAlert(), listingChangedPathsFails("git diff: exit status 128")).
		then(started(), failed(events.InvestigationFailedPush, "git diff: exit status 128")).
		andNothingIsPushed()
}

func TestAFailedPushFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), pushFails("git push: exit status 1: rejected")).
		then(started(), failed(events.InvestigationFailedPush, "git push: exit status 1: rejected")).
		andNoPRIsOpened()
}

func TestCIConfigChangesAreNotPushed(t *testing.T) {
	given(t).
		when(anAlert(), claudeChanges("src/app.js", ".github/workflows/a.yml", ".gitlab-ci.yml", "Jenkinsfile", ".circleci/config.yml", ".travis.yml", ".buildkite/pipeline.yml", "azure-pipelines.yml")).
		then(started(), failed(events.InvestigationFailedPush, "changes CI configuration, so underling won't push it: .github/workflows/a.yml, .gitlab-ci.yml, Jenkinsfile, .circleci/config.yml, .travis.yml, and 2 more")).
		andNothingIsPushed()
}

func TestAFailedPRFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), prFails("create pull request: status 422: Validation Failed")).
		then(started(), failed(events.InvestigationFailedPR, "create pull request: status 422: Validation Failed"))
}

func TestShuttingDownWhileClaudeRunsFailsTheInvestigation(t *testing.T) {
	given(t).
		when(anAlert(), underlingShutsDownWhileClaudeRuns).
		then(started(), failed(events.InvestigationFailedShutdown, "underling shut down while claude was running")).
		andNothingIsPushed()
}

func TestAnyFailureDuringShutdownIsAShutdownFailure(t *testing.T) {
	given(t).
		when(anAlert(), underlingShutsDownDuringThePush).
		then(started(), failed(events.InvestigationFailedShutdown, "git push: context canceled"))
}

func TestAnAlertPickedUpDuringShutdownAddsNothing(t *testing.T) {
	given(t).
		underlingIsShuttingDown().
		when(anAlert()).
		then().
		andNothingIsCloned()
}

func TestOtherEventsAreIgnored(t *testing.T) {
	s := given(t).
		when(eventstore.Event{Position: 1, Type: "NoteRegistered", Timestamp: testNow, Payload: json.RawMessage(`{}`)}).
		then().
		andNothingIsCloned()

	if s.logs.Len() != 0 {
		t.Errorf("logged %q, want nothing", s.logs.String())
	}
}

func TestAnUndecodableAlertIsLoggedAndSkipped(t *testing.T) {
	given(t).
		when(eventstore.Event{Position: 3, Type: events.SentryIssueAlertReceivedType, Timestamp: testNow, Payload: json.RawMessage(`{"issueId":1}`)}).
		then().
		andNothingIsCloned().
		andLogged(map[string]any{"level": "ERROR", "msg": "decode sentry issue alert", "handler": handlerName})
}

func TestNameIsFixed(t *testing.T) {
	if got := (&investigator{}).Name(); got != "sentry-issue-investigator" {
		t.Fatalf("got name %q, want sentry-issue-investigator", got)
	}
}

const (
	testIssueID     = "1117540176"
	testBranch      = "underling/sentry-" + testIssueID
	testPRURL       = "https://github.com/acme/web/pull/7"
	testSummary     = "Root cause: blooopy was never declared. Fix: declare it."
	testMaxAge      = time.Hour
	mappedProject   = 1
	unmappedProject = 99
)

// testNow is when the handler started; alerts stored more than testMaxAge
// before it are stale.
var testNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// spec is one given/when/then scenario.
type spec struct {
	t      *testing.T
	store  *eventstore.Store
	ctx    context.Context
	cancel context.CancelFunc
	infra  *fakeInfra
	logs   *bytes.Buffer
	// before is how many events the store held before the alert.
	before int
}

// given starts a scenario whose issue stream already holds prior.
func given(t *testing.T, prior ...event) *spec {
	t.Helper()
	store, err := eventstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &spec{t: t, store: store, ctx: ctx, cancel: cancel, infra: newFakeInfra(), logs: new(bytes.Buffer), before: len(prior)}
	if len(prior) > 0 {
		var stored []eventstore.NewEvent
		for _, e := range prior {
			stored = append(stored, e.new(t))
		}
		if _, _, err := store.Append(streamID(testIssueID), 0, stored); err != nil {
			t.Fatalf("append given events: %v", err)
		}
	}
	return s
}

// underlingIsShuttingDown cancels the handler's context before the alert.
func (s *spec) underlingIsShuttingDown() *spec {
	s.cancel()
	return s
}

// when hands alert to an investigator whose git, Claude and GitHub act as
// the behaviours say, and as for a fix that can be pushed otherwise.
func (s *spec) when(alert eventstore.Event, behaviours ...behaviour) *spec {
	s.t.Helper()
	for _, b := range behaviours {
		b(s)
	}
	h := &investigator{
		ctx:         s.ctx,
		store:       s.store,
		logger:      slog.New(slog.NewJSONHandler(s.logs, nil)),
		staleBefore: testNow.Add(-testMaxAge),
		repos:       map[int64]repo{mappedProject: {owner: "acme", name: "web"}},
		cloner:      s.infra,
		prs:         s.infra,
	}
	h.Handle(alert)
	return s
}

// then checks that the alert added exactly want to the issue's stream, and
// nothing to any other stream.
func (s *spec) then(want ...event) *spec {
	s.t.Helper()
	all, err := s.store.ReadAfter(0, 1000)
	if err != nil {
		s.t.Fatalf("read events: %v", err)
	}
	added := all[s.before:]
	if len(added) != len(want) {
		s.t.Fatalf("the alert added %s, want %s; logs:\n%s", describeStored(added), describeWanted(s.t, want), s.logs)
	}
	for i, w := range want {
		if added[i].Stream != streamID(testIssueID) || added[i].Type != w.typ || !bytes.Equal(added[i].Payload, w.new(s.t).Payload) {
			s.t.Fatalf("the alert added %s, want %s", describeStored(added), describeWanted(s.t, want))
		}
	}
	return s
}

func (s *spec) andNothingIsCloned() *spec {
	s.t.Helper()
	if s.infra.cloned {
		s.t.Error("cloned the repo, want no clone")
	}
	return s
}

func (s *spec) andNothingIsPushed() *spec {
	s.t.Helper()
	if s.infra.pushed {
		s.t.Error("pushed the branch, want no push")
	}
	return s.andNoPRIsOpened()
}

func (s *spec) andNoPRIsOpened() *spec {
	s.t.Helper()
	if s.infra.pr != nil {
		s.t.Errorf("opened PR %+v, want none", *s.infra.pr)
	}
	return s
}

func (s *spec) andTheWorkspaceIsRemoved() *spec {
	s.t.Helper()
	if !s.infra.removed {
		s.t.Error("left the workspace behind")
	}
	return s
}

// andLogged checks that a log line holds all of attrs.
func (s *spec) andLogged(attrs map[string]any) *spec {
	s.t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(s.logs.String()), "\n") {
		var got map[string]any
		if json.Unmarshal([]byte(line), &got) != nil {
			continue
		}
		matches := true
		for k, v := range attrs {
			if got[k] != v {
				matches = false
			}
		}
		if matches {
			return s
		}
	}
	s.t.Errorf("no log line holds %v; logs:\n%s", attrs, s.logs)
	return s
}

// event is an event the alert should add, or one already on the stream.
type event struct {
	typ     string
	payload any
}

func (e event) new(t *testing.T) eventstore.NewEvent {
	t.Helper()
	payload, err := json.Marshal(e.payload)
	if err != nil {
		t.Fatalf("encode %s: %v", e.typ, err)
	}
	return eventstore.NewEvent{Type: e.typ, Payload: payload}
}

func started() event {
	return event{events.SentryIssueInvestigationStartedType, events.SentryIssueInvestigationStarted{IssueID: testIssueID, Project: mappedProject, Repo: "acme/web", Branch: testBranch}}
}

func prOpened(url string) event {
	return event{events.SentryIssueInvestigationPullRequestOpenedType, events.SentryIssueInvestigationPullRequestOpened{IssueID: testIssueID, PRURL: url}}
}

func noChanges() event {
	return event{events.SentryIssueInvestigationNoChangesType, events.SentryIssueInvestigationNoChanges{IssueID: testIssueID}}
}

func skipped(reason string) event {
	return event{events.SentryIssueInvestigationSkippedType, events.SentryIssueInvestigationSkipped{IssueID: testIssueID, Reason: reason}}
}

func failed(stage, reason string) event {
	return event{events.SentryIssueInvestigationFailedType, events.SentryIssueInvestigationFailed{IssueID: testIssueID, Stage: stage, Reason: reason}}
}

func describeStored(stored []eventstore.Event) string {
	var parts []string
	for _, e := range stored {
		parts = append(parts, e.Stream+": "+e.Type+" "+string(e.Payload))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func describeWanted(t *testing.T, want []event) string {
	var parts []string
	for _, e := range want {
		parts = append(parts, streamID(testIssueID)+": "+e.typ+" "+string(e.new(t).Payload))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// anAlert is a live alert for the mapped project.
func anAlert() eventstore.Event { return alertEvent(mappedProject, testNow) }

// aStaleAlert is an alert stored more than testMaxAge before the handler
// started.
func aStaleAlert() eventstore.Event {
	return alertEvent(mappedProject, testNow.Add(-testMaxAge-time.Minute))
}

// anUnmappedAlert is a live alert for a project mapped to no repo.
func anUnmappedAlert() eventstore.Event { return alertEvent(unmappedProject, testNow) }

// aStaleUnmappedAlert is a stale alert for a project mapped to no repo.
func aStaleUnmappedAlert() eventstore.Event {
	return alertEvent(unmappedProject, testNow.Add(-testMaxAge-time.Minute))
}

func alertEvent(project int64, stored time.Time) eventstore.Event {
	payload, _ := json.Marshal(events.SentryIssueAlertReceived{
		EventID:        "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
		IssueID:        testIssueID,
		Project:        project,
		Title:          "ReferenceError: blooopy is not defined",
		Level:          "error",
		WebURL:         "https://sentry.io/organizations/test-org/issues/" + testIssueID + "/",
		TriggeredRule:  "rule-a",
		Culprit:        "?(runner)",
		ExceptionType:  "ReferenceError",
		ExceptionValue: "blooopy is not defined",
		Frames:         []events.SentryStackFrame{{File: "example.js", Function: "runner", Line: 3}},
	})
	return eventstore.Event{
		Stream:    "sentry-issue-alert-x",
		Version:   1,
		Position:  5,
		Type:      events.SentryIssueAlertReceivedType,
		Timestamp: stored,
		Payload:   payload,
	}
}

func decodeAlert(t *testing.T, e eventstore.Event) events.SentryIssueAlertReceived {
	t.Helper()
	var alert events.SentryIssueAlertReceived
	if err := json.Unmarshal(e.Payload, &alert); err != nil {
		t.Fatal(err)
	}
	return alert
}

// behaviour changes how git, Claude or GitHub act in a scenario.
type behaviour func(*spec)

func cloneFails(reason string) behaviour {
	return func(s *spec) { s.infra.cloneErr = errors.New(reason) }
}

func claudeFails(reason string) behaviour {
	return func(s *spec) { s.infra.claudeErr = errors.New(reason) }
}

func claudeTimesOut(s *spec) {
	s.infra.claudeErr = timeoutError{"claude did not finish within 30m0s"}
}

func claudeCommitsNothing(s *spec) { s.infra.commits = 0 }

func claudeCommitsOn(branches ...string) behaviour {
	return func(s *spec) { s.infra.stray = branches }
}

func claudeChanges(paths ...string) behaviour {
	return func(s *spec) { s.infra.changed = paths }
}

func listingBranchesFails(reason string) behaviour {
	return func(s *spec) { s.infra.strayErr = errors.New(reason) }
}

func countingCommitsFails(reason string) behaviour {
	return func(s *spec) { s.infra.commitsErr = errors.New(reason) }
}

func listingChangedPathsFails(reason string) behaviour {
	return func(s *spec) { s.infra.changedErr = errors.New(reason) }
}

func pushFails(reason string) behaviour {
	return func(s *spec) { s.infra.pushErr = errors.New(reason) }
}

func prFails(reason string) behaviour {
	return func(s *spec) { s.infra.prErr = errors.New(reason) }
}

func underlingShutsDownWhileClaudeRuns(s *spec) {
	s.infra.duringClaude = s.cancel
	s.infra.claudeErr = errors.New("underling shut down while claude was running")
}

func underlingShutsDownDuringThePush(s *spec) {
	s.infra.duringPush = s.cancel
	s.infra.pushErr = errors.New("git push: context canceled")
}

// timeoutError is a Claude run that ran past its time limit.
type timeoutError struct{ msg string }

func (e timeoutError) Error() string { return e.msg }
func (timeoutError) Timeout() bool   { return true }

// fakeInfra stands in for git, Claude and GitHub, and notes what the
// investigator asked of them. By default Claude commits a fix that touches
// no CI config and everything succeeds.
type fakeInfra struct {
	cloneErr     error
	claudeErr    error
	duringClaude func()
	stray        []string
	strayErr     error
	commits      int
	commitsErr   error
	changed      []string
	changedErr   error
	pushErr      error
	duringPush   func()
	prErr        error

	cloned  bool
	prompt  string
	pushed  bool
	pr      *pullRequest
	removed bool
}

func newFakeInfra() *fakeInfra {
	return &fakeInfra{commits: 1, changed: []string{"src/app.js"}}
}

func (f *fakeInfra) clone(_ context.Context, issueID string, target repo, branch string) (run, error) {
	if issueID != testIssueID || target != (repo{owner: "acme", name: "web"}) || branch != testBranch {
		return nil, errors.New("fake: unexpected clone of " + target.String() + " on " + branch + " for " + issueID)
	}
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	f.cloned = true
	return f, nil
}

func (f *fakeInfra) claude(_ context.Context, prompt string) (string, error) {
	f.prompt = prompt
	if f.duringClaude != nil {
		f.duringClaude()
	}
	if f.claudeErr != nil {
		return "", f.claudeErr
	}
	return testSummary, nil
}

func (f *fakeInfra) strayBranches(context.Context) ([]string, error) {
	if f.strayErr != nil {
		return nil, f.strayErr
	}
	return slices.Clone(f.stray), nil
}

func (f *fakeInfra) newCommits(context.Context) (int, error) {
	if f.commitsErr != nil {
		return 0, f.commitsErr
	}
	return f.commits, nil
}

func (f *fakeInfra) changedPaths(context.Context) ([]string, error) {
	if f.changedErr != nil {
		return nil, f.changedErr
	}
	return slices.Clone(f.changed), nil
}

func (f *fakeInfra) push(context.Context) error {
	if f.duringPush != nil {
		f.duringPush()
	}
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = true
	return nil
}

func (f *fakeInfra) defaultBranch() string { return "main" }

func (f *fakeInfra) remove() { f.removed = true }

func (f *fakeInfra) openDraft(_ context.Context, target repo, pr pullRequest) (string, error) {
	if target != (repo{owner: "acme", name: "web"}) {
		return "", errors.New("fake: unexpected PR in " + target.String())
	}
	if f.prErr != nil {
		return "", f.prErr
	}
	f.pr = &pr
	return testPRURL, nil
}
