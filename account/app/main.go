package main

import (
	"app/tracing"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

type deltaModel struct {
	Delta int `json:"delta"`
}

type withdrawalRequestModel struct {
	OrderID     int `json:"order_id"`
	WithDrawSum int `json:"withdrawal_sum"`
}

type withDrawalResponseModel struct {
	OrderID int  `json:"order_id"`
	UserID  int  `json:"user_id"`
	Price   int  `json:"price"`
	Status  bool `json:"status"`
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
	getBalanceTpl          = `SELECT COALESCE(SUM(delta),0) FROM account WHERE user_id=$1 AND status=1`
	prepareOperationTpl    = `INSERT INTO account (user_id, request_id, delta, status) VALUES ($1, $2, 0, 0)`
	updateBalanceTpl       = `UPDATE account SET delta=$3, status=1 WHERE user_id=$1 AND request_id=$2 AND status=0`
	ordersCallbackEndpoint = "http://orders.proj.svc.cluster.local:9000/orders/callback/account"
)

var (
	getbalanceStmt       *sql.Stmt
	prepareOperationStmt *sql.Stmt
	updateBalanceStmt    *sql.Stmt
	tracer               opentracing.Tracer
	closer               io.Closer
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "account-postgresql",
		dbPort: "5432",
		dbName: "accountdb",
		dbUser: "accountuser",
		dbPass: "accountpasswd",
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

	var i int
	for i = 0; i < 5; i++ {
		if err = db.PingContext(ctx); err == nil {
			break
		}
		log.Println("Failed to check db connection:", err)
		time.Sleep(30 * time.Second)
	}
	if i == 5 && err != nil {
		log.Fatal("Failed to check db connection:", err)
	}

	mustPrepareStmts(ctx, db)

	r := mux.NewRouter()

	r.HandleFunc("/account/genreq", reqlog(isAuthenticatedMiddleware(newReq))).Methods("GET")
	r.HandleFunc("/account/get", reqlog(isAuthenticatedMiddleware(get)))
	r.HandleFunc("/account/deposit", reqlog(isAuthenticatedMiddleware(deposit))).Methods("POST")
	r.HandleFunc("/account/withdrawal", reqlog(isAuthenticatedMiddleware(withdrawal))).Methods("POST")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	getbalanceStmt, err = db.PrepareContext(ctx, getBalanceTpl)
	if err != nil {
		panic(err)
	}

	prepareOperationStmt, err = db.PrepareContext(ctx, prepareOperationTpl)
	if err != nil {
		panic(err)
	}

	updateBalanceStmt, err = db.PrepareContext(ctx, updateBalanceTpl)
	if err != nil {
		panic(err)
	}
}

func getbalance(id int) (int, error) {
	balance := 0
	err := getbalanceStmt.QueryRow(id).Scan(&balance)
	return balance, err
}

func updatebalance(uid int, rid string, delta int) error {
	res, err := updateBalanceStmt.Exec(uid, rid, delta)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("balance did not change")
	}
	return err
}

func get(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for current balance", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	id, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	b, err := getbalance(id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"balance":%d}`, b)
}

func newReq(w http.ResponseWriter, r *http.Request) {

	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for new operation", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	uid := headers.Get("X-User-Id")
	rid := headers.Get("X-Request-Id")
	if rid == "" {
		rid = uuid.New().String()
	}
	log.Println("X-Request-Id", rid)
	_, err := prepareOperationStmt.Exec(uid, rid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Add("X-Request-Id", rid)
	w.Header().Add("X-User-Id", uid)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "X-Request-Id: %s\n", rid)
}

func deposit(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for deposit", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	rid := headers.Get("X-Request-Id")
	log.Println("X-Request-Id", rid)
	if rid == "" {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("Got wrong request id")
		return
	}
	uid, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("Got wrong header [X-User-Id]: %s", err)
		return
	}
	d := deltaModel{}
	if err = json.NewDecoder(r.Body).Decode(&d); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("Failed to parse data:", err)
		return
	}
	if d.Delta < 0 {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("Failed to update balance: delta value is negative")
		return
	}
	if err = updatebalance(uid, rid, d.Delta); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Failed to update balance: %s", err)
		return
	}
	w.Write([]byte("Balance changed successfully\n"))
}

func withdrawal(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got request for new withdrawal", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	rid := headers.Get("X-Request-Id")
	log.Println("X-Request-Id", rid)
	if rid == "" {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("Got wrong request id")
		return
	}
	uid, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	wr := withdrawalRequestModel{}
	if err = json.NewDecoder(r.Body).Decode(&wr); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("Failed to parse data:", err)
		return
	}
	b, err := getbalance(uid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to get balance for user [%d]: %s", uid, err)
		return
	}
	wc := &withDrawalResponseModel{
		OrderID: wr.OrderID,
		UserID:  uid,
		Price:   wr.WithDrawSum,
		Status:  false,
	}
	if wr.WithDrawSum < 0 {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("Got negative withdrawal sum")
		sendCallback(spanCtx, wc)
		return
	}
	if wr.WithDrawSum > b {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("There are insufficient funds in the account")
		sendCallback(spanCtx, wc)
		return
	}
	if err = updatebalance(uid, rid, -wr.WithDrawSum); err != nil {
		log.Printf("Failed to change balance for user [%d]: %s\n", uid, err)
		w.WriteHeader(http.StatusInternalServerError)
		sendCallback(spanCtx, wc)
		return
	}
	w.WriteHeader(http.StatusOK)
	wc.Status = true
	sendCallback(spanCtx, wc)
}

func sendCallback(spanCtx opentracing.SpanContext, r *withDrawalResponseModel) {
	span := tracer.StartSpan("sending callback with payment result", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	data, err := json.Marshal(r)
	if err != nil {
		log.Printf("Failed to parse data: %s\n", err)
		return
	}
	reqBody := bytes.NewReader(data)
	req, err := http.NewRequest("POST", ordersCallbackEndpoint, reqBody)
	if err != nil {
		log.Printf("Failed callback request: %s\n", err)
		return
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(r.UserID))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("Failed to call back orders endpoint: %s\n", err)
		return
	}
	defer resp.Body.Close()
}

func isAuthenticatedMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header
		if _, ok := headers["X-User-Id"]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Not authenticated"))
			log.Println("Not authenticated")
			return
		}
		log.Println("Authenticated")
		h.ServeHTTP(w, r)
	}
}

func reqlog(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Got request from: %s\n", r.Host)
		h.ServeHTTP(w, r)
	}
}
