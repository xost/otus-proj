package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
)

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
	createUserTpl = `INSERT INTO auth_user (login, password, email, first_name, last_name) VALUES ($1, $2, $3, $4, $5) returning id`
	getUserTpl    = `SELECT id, login, email, first_name, last_name FROM auth_user WHERE login=$1 AND password=$2`
)

var (
	createUserStmt  *sql.Stmt
	getUserStmt     *sql.Stmt
	getUserListStmt *sql.Stmt
	updateUserStmt  *sql.Stmt
	deleteUserStmt  *sql.Stmt
	SESSIONS        = map[string]userModel{}
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "auth-postgresql",
		dbPort: "5432",
		dbName: "authdb",
		dbUser: "authuser",
		dbPass: "authpasswd",
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

	r.HandleFunc("/sessions", sessions).Methods("GET")
	r.HandleFunc("/register", register).Methods("POST")
	r.HandleFunc("/login", login).Methods("POST")
	r.HandleFunc("/signin", signin).Methods("GET")
	r.HandleFunc("/auth", auth)
	r.HandleFunc("/logout", logout).Methods("GET", "POST")
	r.HandleFunc("/health", health)

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createUserStmt, err = db.PrepareContext(ctx, createUserTpl)
	if err != nil {
		panic(err)
	}

	getUserStmt, err = db.PrepareContext(ctx, getUserTpl)
	if err != nil {
		panic(err)
	}
}

func register(w http.ResponseWriter, r *http.Request) {
	u := &userModel{}
	var err error
	if err = json.NewDecoder(r.Body).Decode(u); err != nil {
		log.Println("Failed to parse user data:", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to parse user data"))
		return
	}
	var id int64
	if id, err = createUser(u); err != nil {
		log.Println("Failed to create new user:", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to create new user"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"id": %d}`, id)
	log.Printf("User with email=%s was created", (*u).Email)
}

func signin(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "Please go to login and provide Login/Password"}`))
	log.Println(`Please go to login and provide Login/Password"}`)
}

func sessions(w http.ResponseWriter, _ *http.Request) {
	var data []byte
	var err error
	if data, err = json.Marshal(SESSIONS); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func login(w http.ResponseWriter, r *http.Request) {
	l := &loginModel{}
	var err error
	if err = json.NewDecoder(r.Body).Decode(l); err != nil {
		log.Println("Failed to parse login data:", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to parse login data"))
		return
	}
	var u *userModel
	if u, err = getUserByCredentials(l); err != nil {
		log.Println("Unauthorized due to:", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	sessionID := createSession(u)
	cookie := http.Cookie{
		Name:     "session_id",
		Value:    sessionID,
		HttpOnly: true,
	}
	http.SetCookie(w, &cookie)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func auth(w http.ResponseWriter, r *http.Request) {
	if sessionID, err := r.Cookie("session_id"); err == nil {
		log.Println("sessionID:", sessionID)
		if userInfo, ok := SESSIONS[sessionID.Value]; ok {
			log.Println("inserInfo:", userInfo)
			w.Header().Set("X-User-Id", strconv.Itoa(userInfo.id))
			w.Header().Set("X-User", userInfo.Login)
			w.Header().Set("X-Email", userInfo.Email)
			w.Header().Set("X-First-Name", userInfo.FirstName)
			w.Header().Set("X-Last-Name", userInfo.LastName)
			w.WriteHeader(http.StatusOK)
			data, _ := json.Marshal(userInfo)
			w.Write(data)
			return
		}
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func logout(w http.ResponseWriter, r *http.Request) {
	if sessionID, err := r.Cookie("session_id"); err == nil {
		delete(SESSIONS, sessionID.Value)
	}
	cookie := http.Cookie{
		Name:    "session_id",
		Value:   "",
		Expires: time.Now(),
	}
	w.WriteHeader(http.StatusOK)
	http.SetCookie(w, &cookie)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "OK"}`))
}

func createUser(u *userModel) (int64, error) {
	var lastID int64
	if err := createUserStmt.QueryRow(
		u.Login,
		u.Password,
		u.Email,
		u.FirstName,
		u.LastName,
	).Scan(&lastID); err != nil {
		return 0, err
	}
	return lastID, nil
}

func getUserByCredentials(l *loginModel) (*userModel, error) {
	rows, err := getUserStmt.Query(l.Login, l.Password)
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, errors.New("there is no user with specified credentials")
	}

	id := new(int)
	login := new(string)
	email := new(string)
	firstName := new(string)
	lastName := new(string)

	if err = rows.Scan(
		id,
		login,
		email,
		firstName,
		lastName,
	); err != nil {
		return nil, err
	}
	return &userModel{
		id:        *id,
		Login:     *login,
		Email:     *email,
		FirstName: *firstName,
		LastName:  *lastName,
	}, nil
}

func createSession(u *userModel) string {
	if u == nil {
		log.Println("Something went wrong, got empty user data")
		return ""
	}
	sessionID := uuid.New().String()
	SESSIONS[sessionID] = *u
	return sessionID
}
