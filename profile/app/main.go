package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
)

type profileModel struct {
	id        int
	AvatarURI string `json:"avatar_uri"`
	Age       int    `json:"age"`
}

type userModel struct {
	id        int
	Login     string `json:"login"`
	Password  string `json:"password"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type loginModel struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

type extendedUserModel struct {
	userModel
	profileModel
}

type configModel struct {
	dbHost string
	dbPort string
	dbName string
	dbUser string
	dbPass string
	host   string
	port   string
}

const (
	getUserTpl    = `SELECT avatar_uri, age FROM user_profile WHERE id=$1 limit 1`
	updateUserTpl = `INSERT INTO user_profile (id, avatar_uri, age) VALUES ($1, $2, $3) ON CONFLICT (id) DO UPDATE SET avatar_uri = excluded.avatar_uri , age = excluded.age`
)

var (
	getUserStmt    *sql.Stmt
	updateUserStmt *sql.Stmt
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "profile-postgresql",
		dbPort: "5432",
		dbName: "profiledb",
		dbUser: "profileuser",
		dbPass: "profilepasswd",
		host:   "0.0.0.0",
		port:   "80",
	}
	dbHost := os.Getenv("DBHOST")
	dbPort := os.Getenv("DBPORT")
	dbName := os.Getenv("DBNAME")
	dbUser := os.Getenv("DBUSER")
	dbPass := os.Getenv("DBPASS")
	host := os.Getenv("HOST")
	port := os.Getenv("PORT")

	dbURI := os.Getenv("DATABASE_URI")
	log.Println("... h43 ... ################")
	log.Println(dbURI)
	log.Println("... h43 ... ################")

	if dbHost != "" {
		cfg.dbHost = dbHost
	}
	if dbPort != "" {
		cfg.dbPort = dbPort
	}
	if dbName != "" {
		cfg.dbName = dbName
	}
	if dbUser != "" {
		cfg.dbUser = dbUser
	}
	if dbPass != "" {
		cfg.dbPass = dbPass
	}
	if host != "" {
		cfg.host = host
	}
	if port != "" {
		cfg.port = port
	}
	return cfg
}

func makeDBConn(cfg *configModel) (*sql.DB, error) {
	pgConnString := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.dbHost, cfg.dbPort, cfg.dbUser, cfg.dbPass, cfg.dbName,
	)
	log.Println("connection string: ", pgConnString)
	db, err := sql.Open("postgres", pgConnString)
	return db, err
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := readConf()

	db, err := makeDBConn(cfg)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer db.Close()

	if err = db.PingContext(ctx); err != nil {
		log.Fatal("Failed to check db connection:", err)
	}

	mustPrepareStmts(ctx, db)

	r := mux.NewRouter()

	// r.HandleFunc("/health", health)
	r.HandleFunc("/profile/me", isAuthenticatedMiddleware(updateMe)).Methods("PUT")
	r.HandleFunc("/profile/me", isAuthenticatedMiddleware(me))

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	getUserStmt, err = db.PrepareContext(ctx, getUserTpl)
	if err != nil {
		panic(err)
	}

	updateUserStmt, err = db.PrepareContext(ctx, updateUserTpl)
	if err != nil {
		panic(err)
	}

}

func health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "OK"}`))
}

func me(w http.ResponseWriter, r *http.Request) {
	headers := r.Header
	id, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	row := getUserStmt.QueryRow(id)
	avatarURL := new(string)
	age := new(int)
	p := profileModel{}
	if err := row.Scan(avatarURL, age); err == nil {
		p.Age = *age
		p.AvatarURI = *avatarURL
	}

	eu := extendedUserModel{
		userModel: userModel{
			id:        id,
			Login:     headers.Get("X-User"),
			Email:     headers.Get("X-Email"),
			FirstName: headers.Get("X-First-Name"),
			LastName:  headers.Get("X-Last-Name"),
		},
		profileModel: p,
	}
	data, _ := json.Marshal(eu)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func updateMe(w http.ResponseWriter, r *http.Request) {
	up := &profileModel{}
	if err := json.NewDecoder(r.Body).Decode(up); err != nil {
		log.Println("Failed to parse data:", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to parse login data"))
		return
	}
	log.Printf("userProfile: %+v\n", up)
	var err error
	if up.id, err = strconv.Atoi(r.Header.Get("X-User-Id")); err != nil {
		panic(err)
	}

	if _, err = updateUserStmt.Query(up.id, up.AvatarURI, up.Age); err != nil {
		log.Println("Internal server error:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	data, err := json.Marshal(up)
	if err != nil {
		log.Println("Internal server error:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func isAuthenticatedMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header
		fmt.Println(headers)
		if _, ok := headers["X-User-Id"]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Not authenticated"))
			return
		}
		h.ServeHTTP(w, r)
	}
}
