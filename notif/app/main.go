package main

import (
	"app/tracing"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

type notifModel struct {
	OrderID int    `json:"order_id"`
	Message string `json:"message"`
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
	createNotifTpl = `INSERT INTO notif (user_id, order_id, message) VALUES ($1, $2, $3) returning id`
	getNotifsTpl   = `SELECT order_id, message FROM notif WHERE user_id=$1`
)

var (
	createNotifStmt *sql.Stmt
	getNotifsStmt   *sql.Stmt
	tracer          opentracing.Tracer
	closer          io.Closer
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "notif-postgresql",
		dbPort: "5432",
		dbName: "notifdb",
		dbUser: "notifuser",
		dbPass: "notifpasswd",
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

	tracer, closer = tracing.Init()
	defer closer.Close()

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

	r.HandleFunc("/notif/create", isAuthenticatedMiddleware(create)).Methods("POST")
	r.HandleFunc("/notif/get", isAuthenticatedMiddleware(get)).Methods("GET")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createNotifStmt, err = db.PrepareContext(ctx, createNotifTpl)
	if err != nil {
		panic(err)
	}

	getNotifsStmt, err = db.PrepareContext(ctx, getNotifsTpl)
	if err != nil {
		panic(err)
	}
}

func createNotif(id int, message string) error {
	_, err := createNotifStmt.Query(id, message)
	if err != nil {
		log.Printf("Failed to create notification for user id [%d]: %s", id, err)
		return err
	}
	return nil
}

func getNotif(uid int) ([]notifModel, error) {
	rows, err := getNotifsStmt.Query(uid)
	if err != nil {
		return nil, err
	}

	orderID := new(int)
	message := new(string)
	var notifs []notifModel
	for rows.Next() {
		if err = rows.Scan(orderID, message); err != nil {
			log.Printf("Failed to scan row: %s", err)
			continue
		}
		notifs = append(notifs, notifModel{})
	}

	return notifs, nil
}

func create(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for new notify", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	id, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	n := notifModel{}
	if err = json.NewDecoder(r.Body).Decode(&n); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id [%d]: %s\n", id, err)
		return
	}
	if err = createNotif(id, n.Message); err != nil {
		log.Printf("Failed to create notification for user id [%d]: %s\n", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully created notification for user id [%d]\n", id)
	w.WriteHeader(http.StatusOK)
}

func get(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for new notify", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	id, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Got wrong header [X-User-Id]: %s", err)
		return
	}
	notifs, _ := getNotif(id)
	data, err := json.Marshal(notifs)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse data: %s", err)
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
