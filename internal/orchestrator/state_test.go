package orchestrator

import (
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

// testState is an in-memory session.State whose Get result (or error) is
// fixed by the test, including the untyped error a real store can return.
type testState struct {
	value any
	err   error
}

func (s testState) Get(string) (any, error) { return s.value, s.err }
func (testState) Set(string, any) error     { return nil }
func (testState) All() iter.Seq2[string, any] {
	return func(func(string, any) bool) {}
}

// testSession wraps a session.State so loadSessionState can be exercised over
// both a present and an absent state store without a live ADK runtime.
type testSession struct {
	state session.State
}

func (testSession) ID() string                { return "session-test" }
func (testSession) AppName() string           { return "app-test" }
func (testSession) UserID() string            { return "user-test" }
func (s testSession) State() session.State    { return s.state }
func (testSession) Events() session.Events    { return nil }
func (testSession) LastUpdateTime() time.Time { return time.Time{} }

// wireState mimics what a JSON-backed session store hands back: the state as
// decoded JSON containers rather than the original Go value.
func wireState(t *testing.T, state CampaignState) any {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var wire any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("decode state wire form: %v", err)
	}
	return wire
}

func TestLoadSessionStateReadsPersistedCampaignState(t *testing.T) {
	want := CampaignState{Request: CampaignRequest{CampaignID: "campaign-1", Repository: "/repo"}, CandidatesTried: 3}

	tests := []struct {
		name    string
		session session.Session
		want    CampaignState
		wantErr string
	}{
		{
			name:    "nil session reports unavailable state",
			session: nil,
			wantErr: "campaign state unavailable: session state is nil",
		},
		{
			name:    "nil state store reports unavailable state",
			session: testSession{state: nil},
			wantErr: "campaign state unavailable: session state is nil",
		},
		{
			name:    "missing state key reports unavailable state",
			session: testSession{state: testState{err: session.ErrStateKeyNotExist}},
			wantErr: "campaign state unavailable: " + session.ErrStateKeyNotExist.Error(),
		},
		{
			name:    "other read failure is reported as a read error",
			session: testSession{state: testState{err: errors.New("disk on fire")}},
			wantErr: "read campaign state: disk on fire",
		},
		{
			name:    "typed value round-trips",
			session: testSession{state: testState{value: want}},
			want:    want,
		},
		{
			name:    "typed pointer round-trips",
			session: testSession{state: testState{value: &want}},
			want:    want,
		},
		{
			name:    "nil typed pointer reports unavailable state",
			session: testSession{state: testState{value: (*CampaignState)(nil)}},
			wantErr: "campaign state unavailable: nil value",
		},
		{
			name:    "wire map is decoded into campaign state",
			session: testSession{state: testState{value: wireState(t, want)}},
			want:    want,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadSessionState(tc.session)
			assertLoadedState(t, got, err, tc.want, tc.wantErr)
		})
	}
}

// assertLoadedState holds the per-case expectations so the table test body
// stays within the complexity gate.
func assertLoadedState(t *testing.T, got CampaignState, err error, want CampaignState, wantErr string) {
	t.Helper()
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("loadSessionState() error = %v, want %q", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("loadSessionState() unexpected error = %v", err)
	}
	// Slices are compared by their decoded identity below; DeepEqual
	// would flag the nil-vs-empty difference the JSON wire form
	// legitimately introduces.
	if got.Request.CampaignID != want.Request.CampaignID || got.CandidatesTried != want.CandidatesTried {
		t.Fatalf("loadSessionState() = %+v, want %+v", got, want)
	}
}

func TestDecodeCampaignStateRejectsUndecodableValues(t *testing.T) {
	want := CampaignState{Request: CampaignRequest{CampaignID: "campaign-2"}}

	t.Run("typed value round-trips", func(t *testing.T) { assertDecodedState(t, want, want) })
	t.Run("typed pointer round-trips", func(t *testing.T) { assertDecodedState(t, &want, want) })
	t.Run("nil typed pointer reports unavailable state", func(t *testing.T) {
		assertDecodeError(t, (*CampaignState)(nil), "nil value")
	})
	t.Run("unencodable value names its type", func(t *testing.T) {
		// A channel is not JSON encodable: the encode branch has to name the
		// type rather than swallow it.
		assertDecodeError(t, make(chan int), "encode campaign state chan int")
	})
	t.Run("undecodable scalar reports decode failure", func(t *testing.T) {
		// A JSON scalar marshals fine but cannot be decoded into CampaignState.
		assertDecodeError(t, 42, "decode campaign state int")
	})
}

func assertDecodedState(t *testing.T, raw any, want CampaignState) {
	t.Helper()
	got, err := decodeCampaignState(raw)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeCampaignState(%T) = %+v, %v", raw, got, err)
	}
}

func assertDecodeError(t *testing.T, raw any, wantErr string) {
	t.Helper()
	if _, err := decodeCampaignState(raw); err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("decodeCampaignState(%T) error = %v, want %q", raw, err, wantErr)
	}
}
