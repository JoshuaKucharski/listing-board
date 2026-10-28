package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

func TestStartMovesTheJobToRunning(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != "running" {
		t.Fatalf("status %s, want running", got.Status)
	}
	if got.RunID == "" {
		t.Fatal("runId is empty")
	}
	for _, step := range got.Checklist {
		if step.State != "pending" {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}

	// Starting the same sheet again while it runs is harmless and keeps the runId.
	res = call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("second start %d: %s", res.Code, res.Body.String())
	}
	if again := decodeJob(t, res); again.RunID != got.RunID {
		t.Fatalf("runId changed from %s to %s", got.RunID, again.RunID)
	}
}

func TestUploadStoresTheBodyUnderUserAndJob(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := whoAmI(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	content := []byte("Market loft, 10 Market Street. Two bedrooms, river view.\n")
	uploadFile(t, srv, cookie, job.ID, "../../escaped.txt", content, "text/plain")

	path := filepath.Join(dir, userID, job.ID, "body")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("brochure was not stored at %s: %v", path, err)
	}
	if !bytes.Equal(data, content) {
		t.Fatal("stored bytes differ from the upload")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "escaped.txt")); err == nil {
		t.Fatal("the client filename escaped the data directory")
	}
	if files := filesUnder(t, dir); len(files) != 1 || files[0] != path {
		t.Fatalf("data dir holds %v, want only %s", files, path)
	}
	res := call(srv, http.MethodGet, "/api/jobs/"+job.ID, cookie, nil, "")
	if got := decodeJob(t, res); got.Filename != "../../escaped.txt" {
		t.Fatalf("filename %q, want the client name kept as data", got.Filename)
	}
}

func TestListHidesOtherUsersJobs(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs", grace, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range jobs {
		if item.ID == job.ID {
			t.Fatal("another account can see this sheet in the list")
		}
	}
}

func TestStartRequiresASession(t *testing.T) {
	srv, _ := newTestServer(t)
	res := call(srv, http.MethodPost, "/api/jobs/"+board.NewID()+"/start", "", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.Code)
	}
}

func TestStartWithoutABrochureReturns400(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.Code)
	}
	if msg := errorText(t, res); msg != "Attach a brochure first" {
		t.Fatalf("error %q, want %q", msg, "Attach a brochure first")
	}
}

func TestOnlyOneSheetPerAccountRunsAtATime(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	first := createSheet(t, srv, ada, "Market loft", "10 Market Street")
	second := createSheet(t, srv, ada, "River flat", "2 Quay Lane")
	uploadFile(t, srv, ada, first.ID, "a.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	uploadFile(t, srv, ada, second.ID, "b.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	if res := call(srv, http.MethodPost, "/api/jobs/"+first.ID+"/start", ada, nil, ""); res.Code != http.StatusOK {
		t.Fatalf("first start %d: %s", res.Code, res.Body.String())
	}
	res := call(srv, http.MethodPost, "/api/jobs/"+second.ID+"/start", ada, nil, "")
	if res.Code != http.StatusConflict {
		t.Fatalf("second start %d, want 409", res.Code)
	}
	if got := decodeJob(t, call(srv, http.MethodGet, "/api/jobs/"+second.ID, ada, nil, "")); got.Status != "draft" || got.RunID != "" {
		t.Fatalf("second sheet is %s with runId %q, want an untouched draft", got.Status, got.RunID)
	}

	// The rule is per account: another account may run its own sheet meanwhile.
	grace := signUp(t, srv, "grace@example.com")
	hers := createSheet(t, srv, grace, "Garden house", "7 Orchard Road")
	uploadFile(t, srv, grace, hers.ID, "c.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	if res := call(srv, http.MethodPost, "/api/jobs/"+hers.ID+"/start", grace, nil, ""); res.Code != http.StatusOK {
		t.Fatalf("other account start %d: %s", res.Code, res.Body.String())
	}
}

func TestAFailedSheetStaysFailed(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	// The worker, not the handler, fails a sheet; put the store in that state directly.
	ctx := context.Background()
	stored, err := srv.store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status = "failed"
	stored.Error = "Brochure is missing"
	stored.RunID = board.NewID()
	if err := srv.store.UpdateJob(ctx, stored); err != nil {
		t.Fatal(err)
	}

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != "failed" || got.Error != "Brochure is missing" || got.RunID != stored.RunID {
		t.Fatalf("got %s %q runId=%s, want the failed sheet unchanged", got.Status, got.Error, got.RunID)
	}
}

func TestSomeoneElsesSheetIs404OnEveryRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")
	uploadFile(t, srv, ada, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	if res := call(srv, http.MethodGet, "/api/jobs/"+job.ID, ada, nil, ""); res.Code != http.StatusOK {
		t.Fatalf("owner cannot read own sheet: %d", res.Code)
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/jobs/" + job.ID},
		{http.MethodPost, "/api/jobs/" + job.ID + "/file"},
		{http.MethodPost, "/api/jobs/" + job.ID + "/start"},
		{http.MethodGet, "/api/jobs/" + job.ID + "/file"},
	}
	for _, route := range routes {
		res := call(srv, route.method, route.path, grace, nil, "")
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s %s as another account: %d, want 404", route.method, route.path, res.Code)
		}
	}
}

func TestASecondUploadReturns400(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "first.txt", []byte("first"), "text/plain")

	res := uploadRaw(t, srv, cookie, job.ID, "second.txt", []byte("second"), "text/plain")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.Code)
	}
	userID := whoAmI(t, srv, cookie)
	data, err := os.ReadFile(filepath.Join(dir, userID, job.ID, "body"))
	if err != nil || string(data) != "first" {
		t.Fatalf("stored body %q err=%v, want the first upload untouched", data, err)
	}
	if got := decodeJob(t, call(srv, http.MethodGet, "/api/jobs/"+job.ID, cookie, nil, "")); got.Filename != "first.txt" {
		t.Fatalf("filename %q, want first.txt", got.Filename)
	}
}

func TestUploadRejectsFilesOverOneMiBWithoutTouchingTheDisk(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := uploadRaw(t, srv, cookie, job.ID, "big.txt", bytes.Repeat([]byte("a"), maxBrochure+1), "text/plain")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.Code)
	}
	if files := filesUnder(t, dir); len(files) != 0 {
		t.Fatalf("rejected upload left files behind: %v", files)
	}
	if got := decodeJob(t, call(srv, http.MethodGet, "/api/jobs/"+job.ID, cookie, nil, "")); got.Filename != "" {
		t.Fatalf("filename %q, want empty", got.Filename)
	}

	// Exactly 1 MiB is still fine.
	uploadFile(t, srv, cookie, job.ID, "limit.txt", bytes.Repeat([]byte("a"), maxBrochure), "text/plain")
}

func TestUploadDecidesTheTypeFromTheBytes(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	cases := []struct {
		name        string
		contentType string
		data        []byte
		want        int
	}{
		{"notes.txt", "text/plain", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"), http.StatusOK},             // PDF bytes win over header and extension
		{"photo.png", "image/png", []byte("A plain text brochure."), http.StatusOK},                     // text bytes win over header and extension
		{"scan.pdf", "application/pdf", []byte{0x89, 'P', 'N', 'G', 0x00, 0x0a}, http.StatusBadRequest}, // binary with NUL
		{"nul.txt", "text/plain", []byte("ab\x00cd"), http.StatusBadRequest},                            // NUL inside text
		{"bad.txt", "text/plain", []byte{0xff, 0xfe, 'x'}, http.StatusBadRequest},                       // invalid UTF-8
		{"empty.txt", "text/plain", []byte{}, http.StatusBadRequest},                                    // empty
	}
	for _, c := range cases {
		job := createSheet(t, srv, cookie, "Sheet "+c.name, "1 Test Street")
		res := uploadRaw(t, srv, cookie, job.ID, c.name, c.data, c.contentType)
		if res.Code != c.want {
			t.Fatalf("%s (%s): status %d, want %d: %s", c.name, c.contentType, res.Code, c.want, res.Body.String())
		}
	}
}

func TestDownloadServesTheBrochureBytes(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	content := []byte("%PDF-1.4\n1 0 obj\n")
	uploadFile(t, srv, cookie, job.ID, "../../escaped.pdf", content, "application/pdf")

	res := call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(res.Body.Bytes(), content) {
		t.Fatal("downloaded bytes differ from the upload")
	}
	if cd := res.Header().Get("Content-Disposition"); cd != `attachment; filename="escaped.pdf"` {
		t.Fatalf("Content-Disposition %q", cd)
	}
}

func TestSheetJSONHasExactlyTheDocumentedFields(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs/"+job.ID, cookie, nil, "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(res.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "title", "address", "status", "error", "runId", "filename", "checklist", "createdAt"}
	if len(raw) != len(want) {
		t.Fatalf("sheet has %d fields, want %d: %v", len(raw), len(want), keys(raw))
	}
	for _, field := range want {
		if _, ok := raw[field]; !ok {
			t.Fatalf("field %q is missing", field)
		}
	}
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(newMem(), dir, ""), dir
}

func signUp(t *testing.T, srv *Server, email string) string {
	t.Helper()
	body := strings.NewReader(`{"email":"` + email + `","password":"password123"}`)
	res := call(srv, http.MethodPost, "/api/register", "", body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("register %d: %s", res.Code, res.Body.String())
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("register did not set a session")
	return ""
}

func whoAmI(t *testing.T, srv *Server, cookie string) string {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/me", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("me %d: %s", res.Code, res.Body.String())
	}
	var acc account
	if err := json.Unmarshal(res.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	return acc.ID
}

func createSheet(t *testing.T, srv *Server, cookie, title, address string) board.Job {
	t.Helper()
	body := strings.NewReader(`{"title":"` + title + `","address":"` + address + `"}`)
	res := call(srv, http.MethodPost, "/api/jobs", cookie, body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func uploadFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) {
	t.Helper()
	res := uploadRaw(t, srv, cookie, id, name, data, contentType)
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
}

// uploadRaw sends a multipart upload and returns the response for tests that
// expect a rejection.
func uploadRaw(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, &buf, writer.FormDataContentType())
}

func decodeJob(t *testing.T, res *httptest.ResponseRecorder) board.Job {
	t.Helper()
	var job board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func errorText(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body["error"]
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func call(srv *Server, method, path, cookie string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	return res
}

type mem struct {
	mu       sync.Mutex
	users    map[string]board.User
	byEmail  map[string]string
	sessions map[string]memSession
	jobs     map[string]board.Job
}

type memSession struct {
	userID  string
	expires time.Time
}

func newMem() *mem {
	return &mem{
		users:    map[string]board.User{},
		byEmail:  map[string]string{},
		sessions: map[string]memSession{},
		jobs:     map[string]board.Job{},
	}
}

func (m *mem) CreateUser(_ context.Context, email, passwordHash string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return board.User{}, board.ErrDuplicate
	}
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	m.users[user.ID] = user
	m.byEmail[email] = user.ID
	return user, nil
}

func (m *mem) UserByEmail(_ context.Context, email string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return m.users[id], nil
}

func (m *mem) CreateSession(_ context.Context, userID, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = memSession{userID: userID, expires: expires}
	return nil
}

func (m *mem) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *mem) UserByToken(_ context.Context, token string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[token]
	if !ok || time.Now().After(session.expires) {
		return board.User{}, board.ErrNotFound
	}
	user, ok := m.users[session.userID]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return user, nil
}

func (m *mem) ListJobs(_ context.Context, userID string) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []board.Job
	for _, job := range m.jobs {
		if job.UserID == userID {
			jobs = append(jobs, cloneJob(job))
		}
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, nil
}

func (m *mem) CreateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func (m *mem) Job(_ context.Context, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return board.Job{}, board.ErrNotFound
	}
	return cloneJob(job), nil
}

func (m *mem) UpdateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[job.ID]; !ok {
		return board.ErrNotFound
	}
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func cloneJob(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
