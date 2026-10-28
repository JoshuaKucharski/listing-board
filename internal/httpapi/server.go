package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"listingboard/internal/board"
)

const (
	sessionCookie = "lb_session"
	// maxBrochure is the largest brochure accepted, 1 MiB.
	maxBrochure = 1 << 20
	// bodyFile is the fixed file name under {DATA_DIR}/{userID}/{jobID}. The
	// client's filename is kept as data on the sheet and never used as a path.
	bodyFile = "body"
)

type Server struct {
	store   board.Store
	dataDir string
	webDir  string
	handler http.Handler
}

type account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func New(store board.Store, dataDir, webDir string) *Server {
	s := &Server{store: store, dataDir: dataDir, webDir: webDir}
	s.handler = withCORS(s.routes())
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/jobs/{id}/file", s.upload)
	mux.HandleFunc("POST /api/jobs/{id}/start", s.start)
	mux.HandleFunc("GET /api/jobs/{id}/file", s.download)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !strings.Contains(email, "@") || len(email) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter an email address")
		return
	}
	if len(body.Password) < 8 || len(body.Password) > 200 {
		writeErr(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	user, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, board.ErrDuplicate) {
		writeErr(w, http.StatusConflict, "An account with that email already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusCreated, account{ID: user.ID, Email: user.Email})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	user, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, board.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not sign in")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load sheets")
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   string `json:"title"`
		Address string `json:"address"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	title := strings.TrimSpace(body.Title)
	address := strings.TrimSpace(body.Address)
	if title == "" || address == "" || len(title) > 200 || len(address) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter a title and an address")
		return
	}
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    user.ID,
		Title:     title,
		Address:   address,
		Status:    "draft",
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the sheet")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// upload accepts the one brochure a sheet may have. The type is decided from
// the bytes, the size limit is enforced before anything touches the disk, and
// the bytes are stored under a fixed name so the client's filename can never
// escape the data directory.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename != "" {
		writeErr(w, http.StatusBadRequest, "This sheet already has a brochure")
		return
	}
	name, data, reject := readBrochure(w, r)
	if reject != "" {
		writeErr(w, http.StatusBadRequest, reject)
		return
	}
	if !brochureAllowed(data) {
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a text file")
		return
	}
	path := s.bodyPath(job)
	if err := writeFile(path, data); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	job.Filename = name
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		_ = os.Remove(path)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// start moves a draft to "running" and hands it to the worker. The runId is
// chosen here, once; the worker never changes it, so a restart continues the
// same run. The handler never sets "success" or "failed".
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	switch job.Status {
	case "running", "success", "failed":
		// Starting again changes nothing: a running sheet keeps its runId,
		// a finished sheet stays finished, a failed sheet stays failed.
		writeJSON(w, http.StatusOK, job)
		return
	}
	if strings.TrimSpace(job.Filename) == "" {
		writeErr(w, http.StatusBadRequest, "Attach a brochure first")
		return
	}
	busy, err := s.otherSheetRunning(r.Context(), user.ID, job.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
		return
	}
	if busy {
		writeErr(w, http.StatusConflict, "Another sheet is already running")
		return
	}
	job.Status = "running"
	job.RunID = board.NewID()
	job.Error = ""
	for i := range job.Checklist {
		job.Checklist[i].State = "pending"
	}
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// otherSheetRunning reports whether the account has a sheet other than
// exceptID in "running". One running sheet per account at a time.
func (s *Server) otherSheetRunning(ctx context.Context, userID, exceptID string) (bool, error) {
	jobs, err := s.store.ListJobs(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, other := range jobs {
		if other.ID != exceptID && other.Status == "running" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	f, err := os.Open(s.bodyPath(job))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	defer f.Close()
	name := filepath.Base(job.Filename)
	w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(name))
	http.ServeContent(w, r, name, job.UpdatedAt, f)
}

// bodyPath is {DATA_DIR}/{userID}/{jobID}/body. Both ids are generated hex,
// so the path is built from trusted parts only.
func (s *Server) bodyPath(job board.Job) string {
	return filepath.Join(s.dataDir, job.UserID, job.ID, bodyFile)
}

// load fetches the sheet named in the URL for the signed-in user. A sheet
// that does not exist and a sheet that belongs to someone else both answer
// 404, so ids cannot be probed.
func (s *Server) load(w http.ResponseWriter, r *http.Request, user board.User) (board.Job, bool) {
	job, err := s.store.Job(r.Context(), r.PathValue("id"))
	if errors.Is(err, board.ErrNotFound) || (err == nil && job.UserID != user.ID) {
		writeErr(w, http.StatusNotFound, "Not found")
		return board.Job{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the sheet")
		return board.Job{}, false
	}
	return job, true
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) (board.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	user, err := s.store.UserByToken(r.Context(), cookie.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	return user, true
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request, user board.User) error {
	token := board.NewToken()
	if err := s.store.CreateSession(r.Context(), user.ID, token, time.Now().Add(24*time.Hour)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	return nil
}

// readBrochure streams the multipart field "file" into memory without going
// through a temporary file. It returns the client's filename, the bytes, and
// a message when the upload must be rejected with 400. The body is capped at
// the brochure limit plus room for the multipart framing, so an oversized
// upload is cut off early instead of being read to the end.
func readBrochure(w http.ResponseWriter, r *http.Request) (name string, data []byte, reject string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBrochure+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, "Choose a brochure file"
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return "", nil, "Choose a brochure file"
		}
		if err != nil {
			return "", nil, "Brochure upload was incomplete"
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		name = filenameFromPart(part)
		data, err = io.ReadAll(io.LimitReader(part, maxBrochure+1))
		_ = part.Close()
		if err != nil {
			return "", nil, "Brochure upload was incomplete"
		}
		if len(data) > maxBrochure {
			return "", nil, "Brochure is too large"
		}
		if strings.TrimSpace(name) == "" {
			return "", nil, "Choose a brochure file"
		}
		return name, data, ""
	}
}

// filenameFromPart returns the filename exactly as the client sent it. It is
// stored on the sheet as data only; see bodyPath for what reaches the disk.
func filenameFromPart(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return name
		}
	}
	return part.FileName()
}

// brochureAllowed decides the type from the bytes alone. The multipart
// Content-Type and the file extension are not consulted.
//   - PDF: the bytes start with "%PDF-".
//   - Text: the whole file is valid UTF-8 and contains no NUL byte.
//   - Anything else, including an empty file, is rejected.
func brochureAllowed(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return true
	}
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// writeFile writes data to path through a temporary name in the same
// directory, so a crash mid-write never leaves a half brochure behind.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == "http://localhost:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
