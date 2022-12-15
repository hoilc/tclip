package main

import (
	"context"
	"crypto/md5"
	"database/sql"
	"database/sql/driver"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/tailscale/sqlite"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

var (
	hostname = flag.String("hostname", envOr("TSNET_HOSTNAME", "paste"), "hostname to use on your tailnet, TSNET_HOSTNAME in the environment")
	dataDir  = flag.String("data-location", dataLocation(), "where data is stored, defaults to DATA_DIR or ~/.config/tailscale/paste")

	//go:embed schema.sql
	sqlSchema string

	//go:embed static
	staticFiles embed.FS

	//go:embed tmpl/*.tmpl
	templateFiles embed.FS
)

const formDataLimit = 64 * 1024 // 64 kilobytes (approx. 32 printed pages of text)

func dataLocation() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return os.Getenv("DATA_DIR")
	}
	return filepath.Join(dir, "tailscale", "paste")
}

func envOr(key, defaultVal string) string {
	if result, ok := os.LookupEnv(key); ok {
		return result
	}
	return defaultVal
}

type Server struct {
	lc    *tailscale.LocalClient
	db    *sql.DB
	tmpls *template.Template
}

func (s *Server) TailnetIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.NotFound(w, r)
		return
	}

	ui, err := upsertUserInfo(r.Context(), s.db, s.lc, r.RemoteAddr)
	if err != nil {
			s.ShowError(w, r, err, http.StatusInternalServerError)
		return
	}

	err = s.tmpls.ExecuteTemplate(w, "create.tmpl", struct {
		UserInfo *tailcfg.UserProfile
		Title    string
	}{
		UserInfo: ui.UserProfile,
		Title:    "Create new paste",
	})
	if err != nil {
		log.Printf("%s: %v", r.RemoteAddr, err)
	}
}

func (s *Server) NotFound(w http.ResponseWriter, r *http.Request) {
	s.tmpls.ExecuteTemplate(w, "notfound.tmpl", struct {
		UserInfo *tailcfg.UserProfile
		Title    string
	}{
		UserInfo: nil,
		Title:    "Not found",
	})
}

func (s *Server) PublicIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.NotFound(w, r)
		return
	}

	s.tmpls.ExecuteTemplate(w, "publicindex.tmpl", struct {
		UserInfo *tailcfg.UserProfile
		Title    string
	}{
		UserInfo: nil,
		Title:    "Not found",
	})
}

func (s *Server) TailnetSubmitPaste(w http.ResponseWriter, r *http.Request) {
	userInfo, err := upsertUserInfo(r.Context(), s.db, s.lc, r.RemoteAddr)
	if err != nil {
		log.Printf("%s: %v", r.RemoteAddr, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		err = r.ParseMultipartForm(formDataLimit)
	} else if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		err = r.ParseForm()
	} else {
		log.Printf("%s: unknown content type: %s", r.RemoteAddr, r.Header.Get("Content-Type"))
		http.Error(w, "bad content-type, should be a form", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("%s: bad form: %v", r.RemoteAddr, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !r.Form.Has("filename") && !r.Form.Has("content") {
		log.Printf("%s", r.Form.Encode())
		log.Printf("%s: posted form without filename and data", r.RemoteAddr)
		http.Error(w, "include form values filename and data", http.StatusBadRequest)
		return
	}

	fname := r.Form.Get("filename")
	data := r.Form.Get("content")
	id := uuid.NewString()

	q := `
INSERT INTO pastes
    ( id
    , created_at
    , user_id
    , filename
    , data
    )
VALUES
    ( ?1
    , ?2
    , ?3
    , ?4
    , ?5
    )`

	_, err = s.db.ExecContext(
		r.Context(),
		q,
		id,
		time.Now(),
		userInfo.UserProfile.ID,
		fname,
		data,
	)
	if err != nil {
			s.ShowError(w, r, err, http.StatusInternalServerError)
		return
	}

	log.Printf("new paste: %s", id)

	switch r.Header.Get("Accept") {
	case "text/plain":
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "http://%s/paste/%s", r.Host, id)
	default:
		http.Redirect(w, r, fmt.Sprintf("http://%s/%s", r.Host, id), http.StatusTemporaryRedirect)
	}

}

func (s *Server) TailnetPasteIndex(w http.ResponseWriter, r *http.Request) {
	userInfo, err := upsertUserInfo(r.Context(), s.db, s.lc, r.RemoteAddr)
	if err != nil {
			s.ShowError(w, r, err, http.StatusInternalServerError)
		return
	}

	_ = userInfo

	type JoinedPasteInfo struct {
		ID                string `json:"id"`
		Filename          string `json:"fname"`
		CreatedAt         string `json:"created_at"`
		PasterDisplayName string `json:"created_by"`
	}

	q := `
SELECT p.id
     , p.filename
     , p.created_at
     , u.display_name
FROM pastes p
INNER JOIN users u
  ON p.user_id = u.id
LIMIT 25
OFFSET ?1
`

	uq := r.URL.Query()
	page := uq.Get("page")
	if page == "" {
		page = "0"
	}

	pageNum, err := strconv.Atoi(page)
	if err != nil {
		log.Printf("%s: invalid ?page: %s: %v", r.RemoteAddr, page, err)
		pageNum = 0
	}

	rows, err := s.db.Query(q, clampToZero(pageNum))
	if err != nil {
			s.ShowError(w, r, err, http.StatusInternalServerError)
		return
	}

	jpis := make([]JoinedPasteInfo, 0, 25)

	defer rows.Close()
	for rows.Next() {
		jpi := JoinedPasteInfo{}

		err := rows.Scan(&jpi.ID, &jpi.Filename, &jpi.CreatedAt, &jpi.PasterDisplayName)
		if err != nil {
			s.ShowError(w, r, err, http.StatusInternalServerError)
			return
		}

		jpis = append(jpis, jpi)
	}

	if len(jpis) == 0 {

	}

	var prev, next *int

	if pageNum != 0 {
		i := pageNum - 1
		prev = &i
	}
	if len(jpis) == 25 {
		i := pageNum + 1
		next = &i
	}

	err = s.tmpls.ExecuteTemplate(w, "listpaste.tmpl", struct {
		UserInfo *tailcfg.UserProfile
		Title    string
		Pastes   []JoinedPasteInfo
		Prev     *int
		Next     *int
		Page     int
	}{
		UserInfo: userInfo.UserProfile,
		Title:    "Pastes",
		Pastes:   jpis,
		Prev:     prev,
		Next:     next,
		Page:     pageNum,
	})
	if err != nil {
		log.Printf("%s: %v", r.RemoteAddr, err)
	}
}

func (s *Server) ShowError(w http.ResponseWriter, r *http.Request, err error, code int) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(code)

	log.Printf("%s: %v", r.RemoteAddr, err)

	if err := s.tmpls.ExecuteTemplate(w, "error.tmpl", struct {
		Title, Error string
		UserInfo     any
	}{
		Title: "Oh noes!",
		Error: err.Error(),
	}); err != nil {
		log.Printf("%s: %v", r.RemoteAddr, err)
	}
}

func clampToZero(i int) int {
	if i <= 0 {
		return 0
	}
	return i
}

func (s *Server) ShowPost(w http.ResponseWriter, r *http.Request) {
	ui, _ := upsertUserInfo(r.Context(), s.db, s.lc, r.RemoteAddr)
	var up *tailcfg.UserProfile
	if ui != nil {
		up = ui.UserProfile
	}

	if r.Method != http.MethodGet {
		http.Error(w, "must GET", http.StatusMethodNotAllowed)
		return
	}

	sp := strings.Split(r.URL.Path, "/")
	sp = sp[2:]

	if len(sp) == 0 {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	id := sp[0]

	q := `
SELECT p.filename
     , p.created_at
     , p.data
     , u.id
     , u.login_name
     , u.display_name
     , u.profile_pic_url
FROM pastes p
INNER JOIN users u
  ON p.user_id = u.id
WHERE p.id = ?1`

	row := s.db.QueryRowContext(r.Context(), q, id)
	var fname, data, userID, userLoginName, userDisplayName, userProfilePicURL string
	var createdAt string

	err := row.Scan(&fname, &createdAt, &data, &userID, &userLoginName, &userDisplayName, &userProfilePicURL)
	if err != nil {
		s.ShowError(w, r, fmt.Errorf("can't find paste %s: %w", id, err), http.StatusInternalServerError)
		return
	}

	if len(sp) != 1 {
		switch sp[1] {
		case "raw":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, data)
			return
		case "dl":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, data)
		default:
			s.NotFound(w, r)
		}
	}

	err = s.tmpls.ExecuteTemplate(w, "showpaste.tmpl", struct {
		UserInfo            *tailcfg.UserProfile
		Title               string
		CreatedAt           string
		PasterDisplayName   string
		PasterProfilePicURL string
		ID                  string
		Data                string
	}{
		UserInfo:            up,
		Title:               fname,
		CreatedAt:           createdAt,
		PasterDisplayName:   userDisplayName,
		PasterProfilePicURL: userProfilePicURL,
		ID:                  id,
		Data:                data,
	})
	if err != nil {
		log.Printf("%s: %v", r.RemoteAddr, err)
	}
}

func main() {
	flag.Parse()

	os.MkdirAll(*dataDir, 0700)
	os.MkdirAll(filepath.Join(*dataDir, "tsnet"), 0700)

	s := &tsnet.Server{
		Hostname: *hostname,
		Dir:      filepath.Join(*dataDir, "tsnet"),
		Logf:     func(string, ...any) {},
	}

	if err := s.Start(); err != nil {
		log.Fatal(err)
	}

	db, err := openDB(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	lc, err := s.LocalClient()
	if err != nil {
		log.Fatal(err)
	}
	_ = lc

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}

	tmpls := template.Must(template.ParseFS(templateFiles, "tmpl/*.tmpl"))

	srv := &Server{lc, db, tmpls}

	tailnetMux := http.NewServeMux()
	tailnetMux.Handle("/static/", http.FileServer(http.FS(staticFiles)))
	tailnetMux.HandleFunc("/paste/", srv.ShowPost)
	tailnetMux.HandleFunc("/paste/list", srv.TailnetPasteIndex)
	tailnetMux.HandleFunc("/api/post", srv.TailnetSubmitPaste)
	tailnetMux.HandleFunc("/", srv.TailnetIndex)

	funnelMux := http.NewServeMux()
	funnelMux.Handle("/static/", http.FileServer(http.FS(staticFiles)))
	funnelMux.HandleFunc("/", srv.PublicIndex)
	funnelMux.HandleFunc("/paste/", srv.ShowPost)

	log.Printf("listening on http://%s", *hostname)
	log.Fatal(http.Serve(ln, tailnetMux))
}

func openDB(dir string) (*sql.DB, error) {
	db := sql.OpenDB(sqlite.Connector("file:"+filepath.Join(dir, "data.db"), func(ctx context.Context, conn driver.ConnPrepareContext) error {
		return sqlite.ExecScript(conn.(sqlite.SQLConn), sqlSchema)
	}, nil))

	err := db.Ping()
	if err != nil {
		return nil, err
	}

	return db, nil
}

func md5Hash(inp string) string {
	h := md5.New()
	return fmt.Sprintf("%x", h.Sum([]byte(inp)))
}

func upsertUserInfo(ctx context.Context, db *sql.DB, lc *tailscale.LocalClient, remoteAddr string) (*apitype.WhoIsResponse, error) {
	userInfo, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}

	if userInfo.UserProfile.LoginName == "tagged-devices" {
		userInfo.UserProfile.ID = tailcfg.UserID(userInfo.Node.ID)
		userInfo.UserProfile.LoginName = userInfo.Node.Hostinfo.Hostname()
		userInfo.UserProfile.DisplayName = fmt.Sprintf("tagged node %s: %s", userInfo.Node.Hostinfo.Hostname(), userInfo.Node.Tags[0])
		userInfo.UserProfile.ProfilePicURL = fmt.Sprintf("https://www.gravatar.com/avatar/%s", md5Hash(userInfo.Node.ComputedNameWithHost))
	}

	q := `
INSERT INTO users
    ( id
    , login_name
    , display_name
    , profile_pic_url
    )
VALUES
    ( ?1
    , ?2
    , ?3
    , ?4
    )
ON CONFLICT DO
  UPDATE SET
      login_name =      ?2
    , display_name =    ?3
    , profile_pic_url = ?4
    `

	_, err = db.ExecContext(
		ctx,
		q,
		userInfo.UserProfile.ID,
		userInfo.UserProfile.LoginName,
		userInfo.UserProfile.DisplayName,
		userInfo.UserProfile.ProfilePicURL,
	)
	if err != nil {
		return nil, err
	}

	return userInfo, nil
}
