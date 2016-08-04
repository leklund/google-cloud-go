// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// TODO(jba): test that OnError is getting called appropriately.

package logging

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/internal/bundler"
	"cloud.google.com/go/internal/testutil"
	ltesting "cloud.google.com/go/preview/logging/internal/testing"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	structpb "github.com/golang/protobuf/ptypes/struct"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	logtypepb "google.golang.org/genproto/googleapis/logging/type"
	logpb "google.golang.org/genproto/googleapis/logging/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

const testLogID = "GO-LOGGING-CLIENT-TEST-LOG"

var (
	client          *Client
	uniqueIDCounter int
	startTime       = time.Now()
	testProjectID   string
	testFilter      string
	errorc          chan error

	// Wait for a short period of time. The production service needs a short
	// delay between writing an entry and reading it back.
	wait func()

	// Adjust the fields of a FullEntry received from the production service
	// before comparing it with the expected result. We can't correctly
	// compare certain fields, like times or server-generated IDs.
	clean func(*Entry)

	// Create a new client with the given project ID.
	newClient func(ctx context.Context, projectID string) *Client
)

// Generate unique IDs so tests don't interfere with each other.
func uniqueID(prefix string) string {
	uniqueIDCounter++
	return fmt.Sprintf("%s-%d-%04d", prefix, startTime.Unix(), uniqueIDCounter) // zero-pad for lexical sort
}

func testNow() time.Time {
	return time.Unix(1000, 0)
}

// If true, this test is using the production service, not a fake.
var integrationTest bool

func TestMain(m *testing.M) {
	flag.Parse() // needed for testing.Short()
	ctx := context.Background()
	testProjectID = testutil.ProjID()
	errorc = make(chan error, 100)
	if testProjectID == "" || testing.Short() {
		integrationTest = false
		if testProjectID != "" {
			log.Print("Integration tests skipped in short mode (using fake instead)")
		}
		testProjectID = "PROJECT_ID"
		wait = func() {}
		clean = func(*Entry) {}
		addr, err := ltesting.NewServer()
		if err != nil {
			log.Fatalf("creating fake server: %v", err)
		}
		now = testNow
		newClient = func(ctx context.Context, projectID string) *Client {
			conn, err := grpc.Dial(addr, grpc.WithInsecure())
			if err != nil {
				log.Fatalf("dialing %q: %v", addr, err)
			}
			c, err := NewClient(ctx, projectID, option.WithGRPCConn(conn))
			if err != nil {
				log.Fatalf("creating client for fake at %q: %v", addr, err)
			}
			return c
		}
	} else {
		integrationTest = true
		// Give the service some time to make written entries available.
		wait = func() { time.Sleep(3 * time.Second) }
		clean = func(e *Entry) {
			// We cannot compare timestamps, so set them to the test time.
			// Also, remove the insert ID added by the service.
			e.Timestamp = testNow().UTC()
			e.InsertID = ""
		}
		ts := testutil.TokenSource(ctx, AdminScope)
		if ts == nil {
			log.Fatal("The project key must be set. See CONTRIBUTING.md for details")
		}
		log.Printf("running integration tests with project %s", testProjectID)
		newClient = func(ctx context.Context, projectID string) *Client {
			c, err := NewClient(ctx, projectID,
				option.WithGRPCDialOption(grpc.WithPerRPCCredentials(oauth.TokenSource{TokenSource: ts})),
				option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, ""))))
			if err != nil {
				log.Fatalf("creating prod client: %v", err)
			}
			return c
		}
	}
	client = newClient(ctx, testProjectID)
	client.OnError = func(e error) { errorc <- e }
	client.DeleteLog(ctx, testLogID) // In case previous tests aborted.
	initMetrics(ctx)
	initSinks(ctx)
	testFilter = fmt.Sprintf(`logName = "projects/%s/logs/%s"`, testProjectID, testLogID)
	exit := m.Run()
	client.Close()
	os.Exit(exit)
}

func TestLoggerCreation(t *testing.T) {
	c := &Client{projectID: "PROJECT_ID"}
	defaultResource := &mrpb.MonitoredResource{Type: "global"}
	defaultBundler := &bundler.Bundler{
		DelayThreshold:       DefaultDelayThreshold,
		BundleCountThreshold: DefaultEntryCountThreshold,
		BundleByteThreshold:  DefaultEntryByteThreshold,
		BundleByteLimit:      0,
		BufferedByteLimit:    DefaultBufferedByteLimit,
	}
	for _, test := range []struct {
		options     []LoggerOption
		wantLogger  *Logger
		wantBundler *bundler.Bundler
	}{
		{nil, &Logger{resource: defaultResource}, defaultBundler},
		{
			[]LoggerOption{Resource(nil), CommonLabels(map[string]string{"a": "1"})},
			&Logger{resource: nil, commonLabels: map[string]string{"a": "1"}},
			defaultBundler,
		},
		{
			[]LoggerOption{DelayThreshold(time.Minute), EntryCountThreshold(99),
				EntryByteThreshold(17), EntryByteLimit(18), BufferedByteLimit(19)},
			&Logger{resource: defaultResource},
			&bundler.Bundler{
				DelayThreshold:       time.Minute,
				BundleCountThreshold: 99,
				BundleByteThreshold:  17,
				BundleByteLimit:      18,
				BufferedByteLimit:    19,
			},
		},
	} {
		gotLogger := c.Logger(testLogID, test.options...)
		if got, want := gotLogger.resource, test.wantLogger.resource; !reflect.DeepEqual(got, want) {
			t.Errorf("%v: resource: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.commonLabels, test.wantLogger.commonLabels; !reflect.DeepEqual(got, want) {
			t.Errorf("%v: commonLabels: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.bundler.DelayThreshold, test.wantBundler.DelayThreshold; got != want {
			t.Errorf("%v: DelayThreshold: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.bundler.BundleCountThreshold, test.wantBundler.BundleCountThreshold; got != want {
			t.Errorf("%v: BundleCountThreshold: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.bundler.BundleByteThreshold, test.wantBundler.BundleByteThreshold; got != want {
			t.Errorf("%v: BundleByteThreshold: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.bundler.BundleByteLimit, test.wantBundler.BundleByteLimit; got != want {
			t.Errorf("%v: BundleByteLimit: got %v, want %v", test.options, got, want)
		}
		if got, want := gotLogger.bundler.BufferedByteLimit, test.wantBundler.BufferedByteLimit; got != want {
			t.Errorf("%v: BufferedByteLimit: got %v, want %v", test.options, got, want)
		}
	}
}

func TestLogSync(t *testing.T) {
	ctx := context.Background()
	lg := client.Logger(testLogID)
	defer deleteLog(ctx, testLogID)
	err := lg.LogSync(ctx, Entry{Payload: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	err = lg.LogSync(ctx, Entry{Payload: "goodbye"})
	if err != nil {
		t.Fatal(err)
	}
	wait()
	got, err := allTestLogEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []*Entry{
		entryForTesting("hello"),
		entryForTesting("goodbye"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("#%d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func entryForTesting(payload interface{}) *Entry {
	return &Entry{
		Timestamp: testNow().UTC(),
		Payload:   payload,
		LogName:   "projects/" + testProjectID + "/logs/" + testLogID,
		Resource:  &mrpb.MonitoredResource{Type: "global"},
	}
}

func allTestLogEntries(ctx context.Context) ([]*Entry, error) {
	var es []*Entry
	it := client.Entries(ctx, Filter(testFilter))
	for {
		e, err := cleanNext(it)
		switch err {
		case nil:
			es = append(es, e)
		case iterator.Done:
			return es, nil
		default:
			return nil, err
		}
	}
}

func cleanNext(it *EntryIterator) (*Entry, error) {
	e, err := it.Next()
	if err != nil {
		return nil, err
	}
	clean(e)
	return e, nil
}

func TestLogAndEntries(t *testing.T) {
	ctx := context.Background()
	payloads := []string{"p1", "p2", "p3", "p4", "p5"}
	lg := client.Logger(testLogID)
	defer deleteLog(ctx, testLogID)
	for _, p := range payloads {
		lg.Log(Entry{Payload: p})
	}
	lg.Flush()
	var want []*Entry
	for _, p := range payloads {
		want = append(want, entryForTesting(p))
	}
	wait()
	it := client.Entries(ctx, Filter(testFilter))
	msg, ok := testutil.TestIteratorNext(want, iterator.Done, func() (interface{}, error) { return cleanNext(it) })
	if !ok {
		t.Fatal(msg)
	}
	// TODO(jba): test exact paging.
}

func TestStandardLogger(t *testing.T) {
	ctx := context.Background()
	lg := client.Logger(testLogID)
	defer deleteLog(ctx, testLogID)
	slg := lg.StandardLogger(Info)

	if slg != lg.StandardLogger(Info) {
		t.Error("There should be only one standard logger at each severity.")
	}
	if slg == lg.StandardLogger(Debug) {
		t.Error("There should be a different standard logger for each severity.")
	}

	slg.Print("info")
	lg.Flush()
	wait()
	got, err := allTestLogEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected non-nil request with one entry; got:\n%+v", got)
	}
	if got, want := got[0].Payload.(string), "info\n"; got != want {
		t.Errorf("payload: got %q, want %q", got, want)
	}
	if got, want := Severity(got[0].Severity), Info; got != want {
		t.Errorf("severity: got %s, want %s", got, want)
	}
}

func TestToProtoStruct(t *testing.T) {
	v := struct {
		Foo string                 `json:"foo"`
		Bar int                    `json:"bar,omitempty"`
		Baz []float64              `json:"baz"`
		Moo map[string]interface{} `json:"moo"`
	}{
		Foo: "foovalue",
		Baz: []float64{1.1},
		Moo: map[string]interface{}{
			"a": 1,
			"b": "two",
			"c": true,
		},
	}

	got, err := toProtoStruct(v)
	if err != nil {
		t.Fatal(err)
	}
	want := &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"foo": {Kind: &structpb.Value_StringValue{v.Foo}},
			"baz": {Kind: &structpb.Value_ListValue{&structpb.ListValue{
				[]*structpb.Value{{Kind: &structpb.Value_NumberValue{1.1}}}}}},
			"moo": {Kind: &structpb.Value_StructValue{
				&structpb.Struct{
					Fields: map[string]*structpb.Value{
						"a": {Kind: &structpb.Value_NumberValue{1}},
						"b": {Kind: &structpb.Value_StringValue{"two"}},
						"c": {Kind: &structpb.Value_BoolValue{true}},
					},
				},
			}},
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	// Non-structs should fail to convert.
	for v := range []interface{}{3, "foo", []int{1, 2, 3}} {
		_, err := toProtoStruct(v)
		if err == nil {
			t.Errorf("%v: got nil, want error", v)
		}
	}
}

func textPayloads(req *logpb.WriteLogEntriesRequest) []string {
	if req == nil {
		return nil
	}
	var ps []string
	for _, e := range req.Entries {
		ps = append(ps, e.GetTextPayload())
	}
	return ps
}

func TestFromLogEntry(t *testing.T) {
	res := &mrpb.MonitoredResource{Type: "global"}
	ts, err := ptypes.TimestampProto(testNow())
	if err != nil {
		t.Fatal(err)
	}
	logEntry := logpb.LogEntry{
		LogName:   "projects/PROJECT_ID/logs/LOG_ID",
		Resource:  res,
		Payload:   &logpb.LogEntry_TextPayload{"hello"},
		Timestamp: ts,
		Severity:  logtypepb.LogSeverity_INFO,
		InsertId:  "123",
		HttpRequest: &logtypepb.HttpRequest{
			RequestMethod:                  "GET",
			RequestUrl:                     "http:://example.com/path?q=1",
			RequestSize:                    100,
			Status:                         200,
			ResponseSize:                   25,
			UserAgent:                      "user-agent",
			RemoteIp:                       "127.0.0.1",
			Referer:                        "referer",
			CacheHit:                       true,
			CacheValidatedWithOriginServer: true,
		},
		Labels: map[string]string{
			"a": "1",
			"b": "two",
			"c": "true",
		},
	}
	u, err := url.Parse("http:://example.com/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	want := &Entry{
		LogName:   "projects/PROJECT_ID/logs/LOG_ID",
		Resource:  res,
		Timestamp: testNow().In(time.UTC),
		Severity:  Info,
		Payload:   "hello",
		Labels: map[string]string{
			"a": "1",
			"b": "two",
			"c": "true",
		},
		InsertID: "123",
		HTTPRequest: &HTTPRequest{
			Request: &http.Request{
				Method: "GET",
				URL:    u,
				Header: map[string][]string{
					"User-Agent": []string{"user-agent"},
					"Referer":    []string{"referer"},
				},
			},
			RequestSize:                    100,
			Status:                         200,
			ResponseSize:                   25,
			RemoteIP:                       "127.0.0.1",
			CacheHit:                       true,
			CacheValidatedWithOriginServer: true,
		},
	}
	got, err := fromLogEntry(&logEntry)
	if err != nil {
		t.Fatal(err)
	}
	// Test sub-values separately because %+v and %#v do not follow pointers.
	// TODO(jba): use a differ or pretty-printer.
	if !reflect.DeepEqual(got.HTTPRequest.Request, want.HTTPRequest.Request) {
		t.Fatalf("HTTPRequest.Request:\ngot  %+v\nwant %+v", got.HTTPRequest.Request, want.HTTPRequest.Request)
	}
	if !reflect.DeepEqual(got.HTTPRequest, want.HTTPRequest) {
		t.Fatalf("HTTPRequest:\ngot  %+v\nwant %+v", got.HTTPRequest, want.HTTPRequest)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FullEntry:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestListLogEntriesRequest(t *testing.T) {
	for _, test := range []struct {
		opts       []EntriesOption
		projectIDs []string
		filter     string
		orderBy    string
	}{
		// Default is client's project ID, empty filter and orderBy.
		{nil,
			[]string{"PROJECT_ID"}, "", ""},
		{[]EntriesOption{OrderBy("o"), Filter("f")},
			[]string{"PROJECT_ID"}, "f", "o"},
		{[]EntriesOption{ProjectIDs([]string{"foo"})},
			[]string{"foo"}, "", ""},
		{[]EntriesOption{OrderBy("o"), Filter("f"), ProjectIDs([]string{"foo"})},
			[]string{"foo"}, "f", "o"},
		{[]EntriesOption{OrderBy("o"), Filter("f"), ProjectIDs([]string{"foo"})},
			[]string{"foo"}, "f", "o"},
		// If there are repeats, last one wins.
		{[]EntriesOption{OrderBy("o"), Filter("no"), ProjectIDs([]string{"foo"}), Filter("f")},
			[]string{"foo"}, "f", "o"},
	} {
		got := listLogEntriesRequest("PROJECT_ID", test.opts)
		want := &logpb.ListLogEntriesRequest{
			ProjectIds: test.projectIDs,
			Filter:     test.filter,
			OrderBy:    test.orderBy,
		}
		if !proto.Equal(got, want) {
			t.Errorf("%v:\ngot  %v\nwant %v", test.opts, got, want)
		}
	}
}

func TestSeverity(t *testing.T) {
	if got, want := Info.String(), "Info"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := Severity(-99).String(), "-99"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestErrors(t *testing.T) {
	// Drain errors already seen.
loop:
	for {
		select {
		case <-errorc:
		default:
			break loop
		}
	}
	// Try to log something that can't be JSON-marshalled.
	lg := client.Logger(testLogID)
	lg.Log(Entry{Payload: func() {}})
	// Expect an error.
	select {
	case <-errorc: // pass
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected an error but timed out")
	}
}

type badTokenSource struct{}

func (badTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{}, nil
}

func TestPing(t *testing.T) {
	// Ping twice, in case the service's InsertID logic messes with the error code.
	ctx := context.Background()
	// The global client should be valid.
	if err := client.Ping(ctx); err != nil {
		t.Errorf("project %s: got %v, expected nil", testProjectID, err)
	}
	if err := client.Ping(ctx); err != nil {
		t.Errorf("project %s, #2: got %v, expected nil", testProjectID, err)
	}
	// nonexistent project
	c := newClient(ctx, testProjectID+"-BAD")
	if err := c.Ping(ctx); err == nil {
		t.Errorf("nonexistent project: want error pinging logging api, got nil")
	}
	if err := c.Ping(ctx); err == nil {
		t.Errorf("nonexistent project, #2: want error pinging logging api, got nil")
	}

	// Bad creds. We cannot test this with the fake, since it doesn't do auth.
	if integrationTest {
		c, err := NewClient(ctx, testProjectID,
			option.WithGRPCDialOption(grpc.WithPerRPCCredentials(oauth.TokenSource{TokenSource: badTokenSource{}})),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, ""))))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Ping(ctx); err == nil {
			t.Errorf("bad creds: want error pinging logging api, got nil")
		}
		if err := c.Ping(ctx); err == nil {
			t.Errorf("bad creds, #2: want error pinging logging api, got nil")
		}
		if err := c.Close(); err != nil {
			t.Fatalf("error closing client: %v", err)
		}
	}
}

func TestFromHTTPRequest(t *testing.T) {
	const testURL = "http:://example.com/path?q=1"
	u, err := url.Parse(testURL)
	if err != nil {
		t.Fatal(err)
	}
	req := &HTTPRequest{
		Request: &http.Request{
			Method: "GET",
			URL:    u,
			Header: map[string][]string{
				"User-Agent": []string{"user-agent"},
				"Referer":    []string{"referer"},
			},
		},
		RequestSize:                    100,
		Status:                         200,
		ResponseSize:                   25,
		RemoteIP:                       "127.0.0.1",
		CacheHit:                       true,
		CacheValidatedWithOriginServer: true,
	}
	got := fromHTTPRequest(req)
	want := &logtypepb.HttpRequest{
		RequestMethod:                  "GET",
		RequestUrl:                     testURL,
		RequestSize:                    100,
		Status:                         200,
		ResponseSize:                   25,
		UserAgent:                      "user-agent",
		RemoteIp:                       "127.0.0.1",
		Referer:                        "referer",
		CacheHit:                       true,
		CacheValidatedWithOriginServer: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// deleteLog is used to clean up a log after a test that writes to it.
func deleteLog(ctx context.Context, logID string) {
	client.DeleteLog(ctx, logID)
	wait() // DeleteLog can take some time to happen.
}
