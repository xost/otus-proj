package main

import (
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

	"app/tracing"

	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

type orderModel struct {
	ID      int `json:"id"`
	UserID  int `json:"user_id"`
	EventID int `json:"event_id"`
	Price   int `json:"price,omitempty"`
	Status  int `json:"status,omitempty"`
}

type callbackOccupyModel struct {
	OrderID int  `json:"order_id"`
	UserID  int  `json:"user_id"`
	Price   int  `json:"price"`
	Status  bool `json:"status"`
}

type callbackPaymentModel struct {
	OrderID int  `json:"order_id"`
	UserID  int  `json:"user_id"`
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
	statusCreated = iota
	statusNeedToOccupy
	statusOccupied
	statusNeedToPay
	StatusPaid
	statusCompleted
	statusCancelled = -1
)

const (
	createOrderTpl              = `INSERT INTO orders (user_id, event_id, price, status) VALUES ($1, $2, 0,0) returning id`
	updateStatusTpl             = `UPDATE orders SET status=$2 WHERE id=$1`
	setPriceTpl                 = `UPDATE orders SET price=$2 WHERE id=$1`
	getOrderTpl                 = `SELECT id, user_id, event_id, price, status FROM orders WHERE id=$1`
	getOrdersTpl                = `SELECT id, user_id, event_id, price, status FROM orders WHERE user_id=$1`
	occupySlotEndpoint          = "http://events.proj.svc.cluster.local:9000/events/occupy"
	cancelSlotEndpoint          = "http://events.proj.svc.cluster.local:9000/events/cancel"
	paymentSlotEndpoint         = "http://account.proj.svc.cluster.local:9000/account/withdrawal"
	paymentNewOperationEndpoint = "http://account.proj.svc.cluster.local:9000/account/genreq"
	notifyEndpoint              = "http://notif.proj.svc.cluster.local:9000/notif/create"
	occupySlotTpl               = `{"order_id":%d,"event_id":%d}`
	payTpl                      = `{"order_id":%d,"withdrawal_sum":%d}`
	notifyTpl                   = `{"order_id":%d,"message":"%s"}`
)

var (
	createOrderStmt  *sql.Stmt
	updateStatusStmt *sql.Stmt
	setPriceStmt     *sql.Stmt
	getStatusStmt    *sql.Stmt
	getOrderStmt     *sql.Stmt
	getOrdersStmt    *sql.Stmt
	tracer           opentracing.Tracer
	closer           io.Closer
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "",
		dbPort: "5432",
		dbName: "",
		dbUser: "",
		dbPass: "",
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

	r.HandleFunc("/orders/get", reqlog(isAuthenticatedMiddleware(get))).Methods("GET")
	r.HandleFunc("/orders/create", reqlog(isAuthenticatedMiddleware(create))).Methods("POST")
	r.HandleFunc("/orders/callback/events", reqlog(isAuthenticatedMiddleware(callbackEvents))).Methods("POST")
	r.HandleFunc("/orders/callback/account", reqlog(isAuthenticatedMiddleware(callbackPayment))).Methods("POST")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createOrderStmt, err = db.PrepareContext(ctx, createOrderTpl)
	if err != nil {
		panic(err)
	}

	updateStatusStmt, err = db.PrepareContext(ctx, updateStatusTpl)
	if err != nil {
		panic(err)
	}

	setPriceStmt, err = db.PrepareContext(ctx, setPriceTpl)
	if err != nil {
		panic(err)
	}

	getOrderStmt, err = db.PrepareContext(ctx, getOrderTpl)
	if err != nil {
		panic(err)
	}

	getOrdersStmt, err = db.PrepareContext(ctx, getOrdersTpl)
	if err != nil {
		panic(err)
	}

}

func order(spanCtx opentracing.SpanContext, userID int, o *orderModel) (int, error) {
	span := tracer.StartSpan("querying for order from DB", opentracing.ChildOf(spanCtx))
	defer span.Finish()

	id := new(int)
	err := createOrderStmt.QueryRow(userID, o.EventID).Scan(id)
	return *id, err
}

func getOrder(oid int) (*orderModel, error) {
	o := orderModel{}
	err := getOrderStmt.QueryRow(oid).Scan(&o.ID, &o.UserID, &o.EventID, &o.Price, &o.Status)
	return &o, err
}

func cancelOrder(spanCtx opentracing.SpanContext, oid int) error {
	span := tracer.StartSpan("canceling the order", opentracing.ChildOf(spanCtx))
	defer span.Finish()

	_, err := updateStatusStmt.Exec(oid, statusCancelled)
	if err != nil {
		ext.LogError(span, err)
	}
	_ = modifyOrderStatus(oid, statusCancelled)
	return err
}

func modifyOrderStatus(oid, status int) error {
	_, err := updateStatusStmt.Exec(oid, status)
	return err
}

func setOrderPrice(oid, price int) error {
	_, err := setPriceStmt.Exec(oid, price)
	return err
}

func actionOrderStatus(spanCtx opentracing.SpanContext, oid int) error {
	o, err := getOrder(oid)
	if err != nil {
		log.Printf("Failed to get order [%d]: %s\n", oid, err)
		return err
	}
	switch o.Status {
	case statusCreated:
		log.Println("Order is created, now we need to occupy the slot")
		modifyOrderStatus(oid, statusNeedToOccupy)
		if err = actionOrderStatus(spanCtx, oid); err != nil {
			if err = cancelOrder(spanCtx, o.ID); err != nil {
				log.Printf("Failed to cancel order [%d]\n", o.ID)
			}
			log.Printf("Failed to perform action for order [%d] with status [%d]:%s\n", oid, statusNeedToOccupy, err)
		}
	case statusCancelled:
		log.Println("Order is canceled, do nothing")
	case statusNeedToOccupy:
		log.Printf("Order [%d] is created, now need to occupy slot\n", o.ID)
		if err = occupySlot(spanCtx, o.ID, o.EventID, o.UserID); err != nil {
			log.Printf("Failed to occupy slot for event [%d] for user [%d], need to cancel order. Error: %s\n", o.EventID, o.UserID, err)
			notify(spanCtx, o.UserID, o.ID, "Failed to occupy slot, canceling order")
			if err = cancelOrder(spanCtx, o.ID); err != nil {
				log.Printf("Failed to cancel order [%d]\n", o.ID)
			}
		}
	case statusOccupied:
		log.Println("Slot is occupied, now we need to pay for order")
		modifyOrderStatus(oid, statusNeedToPay)
		if err = actionOrderStatus(spanCtx, oid); err != nil {
			if err = cancelOrder(spanCtx, o.ID); err != nil {
				log.Printf("Failed to cancel order [%d]\n", o.ID)
			}
			log.Printf("Failed to perform action for order [%d] with status [%d]:%s\n", oid, statusNeedToOccupy, err)
		}
	case statusNeedToPay:
		log.Println("Event's slot is occupied, so we need to pay for event")
		if err = payForOrder(spanCtx, o); err != nil { // i need to know price for event, so i have to get it from events service
			log.Printf("Failed to pay the for event [%d] for user [%d], need to cancel order: %s\n", o.EventID, o.UserID, err)
			// also we have to cancel slot, but not now
			notify(spanCtx, o.UserID, o.ID, "Failed to pay for order, canceling order and slot")
			if err = cancelOrder(spanCtx, o.ID); err != nil {
				log.Printf("Failed to cancel order [%d]: %s\n", o.ID, err)
			}
			if err = cancelSlot(spanCtx, o); err != nil {
				log.Printf("Failed to cancel slot [%d]: %s\n", o.ID, err)
			}
		}
	case StatusPaid:
		log.Println("Event's slot is paid, so the order is complete")
		notify(spanCtx, o.UserID, o.ID, "Order was successfully completed")
		// need to notify here
	default:
		log.Println("This should not be happen never")
	}
	return err
}

func get(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("getting user's orders list", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	uid, err := getUserID(r)
	if err != nil {
		log.Printf("Failed to get user id: %s\n", err)
	}
	rows, err := getOrdersStmt.Query(uid)
	if err != nil {
		log.Printf("Failed to get orders list: %s\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	id := new(int)
	user_id := new(int)
	event_id := new(int)
	price := new(int)
	status := new(int)
	orders := make([]orderModel, 0)
	for rows.Next() {
		err := rows.Scan(id, user_id, event_id, price, status)
		if err != nil {
			log.Println("Failed to scan current row:", err)
		}
		orders = append(orders, orderModel{
			ID:      *id,
			UserID:  *user_id,
			EventID: *event_id,
			Price:   *price,
			Status:  *status,
		})
	}
	data, _ := json.Marshal(orders)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func create(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("creating new order", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	headers := r.Header
	userID, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	b := orderModel{}
	if err = json.NewDecoder(r.Body).Decode(&b); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	oid, err := order(spanCtx, userID, &b)
	if err != nil {
		log.Printf("Failed to order event [%d] for user [%d]: %s\n", b.EventID, userID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully ordered event [%d] for user [%d]\n", b.EventID, userID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("The order was successfully create"))
	if err = actionOrderStatus(spanCtx, oid); err != nil {
		log.Printf("Failed to perform action based on order's status: %s\n", err)
	}
}

func occupySlot(spanCtx opentracing.SpanContext, bid, eid, uid int) error {
	span := tracer.StartSpan("sending occupy slot request", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(occupySlotTpl, bid, eid)))
	req, err := http.NewRequest(http.MethodPost, occupySlotEndpoint, bodyReader)
	if err != nil {
		return err
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(uid))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to occupy slot")
	}
	return nil
}

func payForOrder(spanCtx opentracing.SpanContext, b *orderModel) error {
	span := tracer.StartSpan("paying for order request request", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	c := http.Client{}

	req, err := http.NewRequest(http.MethodGet, paymentNewOperationEndpoint, bytes.NewReader([]byte{}))
	if err != nil {
		log.Printf("Failed make request [%s] endpoint: %s", paymentSlotEndpoint, err)
		return err
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(b.UserID))
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("Failed to request [%s] endpoint: %s", paymentNewOperationEndpoint, err)
		return err
	}
	resp.Body.Close()

	rid := resp.Header.Get("X-Request-Id")
	if rid == "" {
		log.Println("Failed to prepare new account operation")
		return errors.New("failed to prepare new account operation")
	}

	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(payTpl, b.ID, b.Price)))
	req, err = http.NewRequest(http.MethodPost, paymentSlotEndpoint, bodyReader)
	if err != nil {
		log.Printf("Failed make request [%s] endpoint: %s", paymentSlotEndpoint, err)
		return err
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(b.UserID))
	req.Header.Set("X-Request-Id", rid)
	resp, err = c.Do(req)
	if err != nil {
		log.Printf("Failed to request [%s] endpoint: %s", paymentSlotEndpoint, err)
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to pay for order, got response status: " + resp.Status)
	}
	return nil
}

func notify(spanCtx opentracing.SpanContext, uid, oid int, message string) {
	span := tracer.StartSpan("sending request for notify", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(notifyTpl, oid, message)))
	req, err := http.NewRequest(http.MethodPost, notifyEndpoint, bodyReader)
	if err != nil {
		log.Printf("Failed to create request endpoint [%s] for new notfy: %s", notifyEndpoint, err)
		return
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(uid))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("Failed to create send endpoint [%s] for new notfy: %s", notifyEndpoint, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Println("Got bad response from notif service:", resp.Status)
	}
}

func cancelSlot(spanCtx opentracing.SpanContext, o *orderModel) error {
	span := tracer.StartSpan("canceling slot request", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(occupySlotTpl, o.ID, o.EventID)))
	req, err := http.NewRequest(http.MethodPost, cancelSlotEndpoint, bodyReader)
	if err != nil {
		return err
	}
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))
	req.Header.Set("X-User-Id", strconv.Itoa(o.UserID))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to cancel slot")
	}
	return nil
}

func callbackEvents(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got callback from [events] service", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	c := callbackOccupyModel{}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if c.Status {
		if err := modifyOrderStatus(c.OrderID, statusOccupied); err != nil {
			log.Printf("Failed to set order price:%s\n", err)
		}
		if err := setOrderPrice(c.OrderID, c.Price); err != nil {
			log.Printf("Failed to set order price:%s Cancel the order\n", err)
			_ = modifyOrderStatus(c.OrderID, statusCancelled)
		}
		if err := actionOrderStatus(spanCtx, c.OrderID); err != nil {
			log.Printf("Failed to action for current order's status\n")
		}
		return
	}
	log.Printf("Failed to occupy event's slot, order will canceled")
	notify(spanCtx, c.UserID, c.OrderID, "Failed to occupy slot, canceling order")
	if err := cancelOrder(spanCtx, c.OrderID); err != nil {
		log.Printf("Failed to cancel order [%d]\n", c.OrderID)
	}
}

func callbackPayment(w http.ResponseWriter, r *http.Request) {
	spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	span := tracer.StartSpan("got callback from [account] service", ext.RPCServerOption(spanCtx))
	defer span.Finish()

	c := callbackPaymentModel{}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if c.Status {
		modifyOrderStatus(c.OrderID, StatusPaid)

		if err := actionOrderStatus(spanCtx, c.OrderID); err != nil {
			log.Printf("Failed to action for current order's status\n")
		}
		return
	}
	log.Printf("Failed to pay event's slot, order will canceled")
	notify(spanCtx, c.UserID, c.OrderID, "Failed to pay for order, canceling order and slot")
	if err := cancelOrder(spanCtx, c.OrderID); err != nil {
		log.Printf("Failed to cancel order [%d]\n", c.OrderID)
	}
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
		h.ServeHTTP(w, r)
	}
}

func reqlog(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Got request from: %s\n", r.Host)
		h.ServeHTTP(w, r)
	}
}

func getUserID(r *http.Request) (int, error) {
	return strconv.Atoi(r.Header.Get("X-User-Id"))
}
