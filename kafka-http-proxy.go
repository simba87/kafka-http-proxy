/*
* Copyright (C) 2015 Alexey Gladkov <gladkov.alexey@gmail.com>
*
* This file is covered by the GNU General Public License,
* which should be included with kafka-http-proxy as the file COPYING.
 */

package main

import (
	"code.google.com/p/gcfg"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/orofarne/hmetrics2"
	"github.com/orofarne/hmetrics2/expvarexport"

	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	addr    = flag.String("addr", "", "The address to bind to")
	brokers = flag.String("brokers", os.Getenv("KAFKA_PEERS"), "The Kafka brokers to connect to, as a comma separated list")
	config  = flag.String("config", "", "Path to configuration file")
	verbose = flag.Bool("verbose", false, "Turn on logging")
)

type JSONResponse struct {
	Status string      `json:"status"`
	Data   interface{} `json:"data"`
}

type KafkaParameters struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
}

type ResponseMessages struct {
	Query    KafkaParameters   `json:"query"`
	Messages []json.RawMessage `json:"messages"`
}

type ResponsePartitionInfo struct {
	Topic        string  `json:"topic"`
	Partition    int32   `json:"partition"`
	Leader       int32   `json:"leader"`
	OffsetOldest int64   `json:"offsetfrom"`
	OffsetNewest int64   `json:"offsetto"`
	Writable     bool    `json:"writable"`
	ReplicasNum  int     `json:"replicasnum"`
	Replicas     []int32 `json:"replicas"`
}

type ResponseTopicListInfo struct {
	Topic      string `json:"topic"`
	Partitions int    `json:"partitions"`
}

type ConnTrack struct {
	ConnID int64
	Conns  int64
}

type Server struct {
	Cfg Config

	Logfile *Logfile
	Pidfile *Pidfile
	Client  *KafkaClient

	lastConnID int64
	connsCount int64

	Cache struct {
		sync.RWMutex

		lastMetadata       *KafkaMetadata
		lastUpdateMetadata int64
	}

	Stats struct {
		ResponsePostTime *hmetrics2.Histogram
		ResponseGetTime  *hmetrics2.Histogram
		HTTPStatus       map[int]*hmetrics2.Counter
	}
}

func (s *Server) Close() error {
	return nil
}

func (s *Server) newConnTrack(r *http.Request) ConnTrack {
	cl := ConnTrack{
		ConnID: atomic.AddInt64(&s.lastConnID, 1),
	}

	conns := atomic.AddInt64(&s.connsCount, 1)

	if s.Cfg.Global.Verbose {
		log.Printf("Opened connection %d (total=%d) [%s %s]", cl.ConnID, conns, r.Method, r.URL)
	}

	cl.Conns = conns
	return cl
}

func (s *Server) closeConnTrack(cl ConnTrack) {
	conns := atomic.AddInt64(&s.connsCount, -1)

	if s.Cfg.Global.Verbose {
		log.Printf("Closed connection %d (total=%d)", cl.ConnID, conns)
	}
}

func (s *Server) writeResponse(w http.ResponseWriter, status int, v *JSONResponse) {
	w.Header().Set("Content-Type", "application/json")

	b, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Unable to marshal result: %v", err)
	}

	w.WriteHeader(status)
	w.Write(b)
}

func (s *Server) successResponse(w http.ResponseWriter, m interface{}) {
	resp := &JSONResponse{
		Status: "success",
		Data:   m,
	}
	s.writeResponse(w, http.StatusOK, resp)
	s.Stats.HTTPStatus[http.StatusOK].Inc()
}

func (s *Server) errorResponse(w http.ResponseWriter, status int, format string, args ...interface{}) {
	resp := &JSONResponse{
		Status: "error",
		Data:   fmt.Sprintf(format, args...),
	}
	if s.Cfg.Global.Verbose {
		log.Printf("Error [%d]: %s\n", status, resp.Data)
	}
	s.writeResponse(w, status, resp)
	s.Stats.HTTPStatus[status].Inc()
}

func (s *Server) RootHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, _ := template.New("help").Parse(`<!DOCTYPE html>
<html>
  <head>
    <meta charset="utf-8">
    <link href="http://yastatic.net/bootstrap/3.3.1/css/bootstrap.min.css" rel="stylesheet">
    <title>Endpoints | Kafka API v1</title>
  </head>
  <body>
    <div class="container"><h2>Kafka API v1</h2><br>
        <table class="table">
          <tr>
            <th class="text-right">Write to Kafka</p></th>
            <td>POST</td>
            <td><code>{schema}://{host}/v1/topics/{topic}/{partition}</code></td>
          </tr>
          <tr>
            <th class="text-right">Read from Kafka</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/topics/{topic}/{partition}?offset={offset}&limit={limit}</code></td>
          </tr>
          <tr>
            <th class="text-right">Obtain topic list</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics</code></td>
          </tr>
          <tr>
            <th class="text-right">Obtain information about all partitions in topic</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics/{topic}</code></td>
          </tr>
          <tr>
            <th class="text-right">Obtain information about partition</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics/{topic}/{partition}</code></td>
          </tr>
        </table>
    </div>
  </body>
</html>`)
	t.Execute(w, nil)
}

func (s *Server) PingHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	w.WriteHeader(http.StatusOK)
}

func (s *Server) NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	s.errorResponse(w, http.StatusNotFound, "404 page not found")
}

func (s *Server) SendHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	if s.Cfg.Global.MaxConns > 0 && cl.Conns >= s.Cfg.Global.MaxConns {
		s.errorResponse(w, http.StatusServiceUnavailable, "Too many connections")
		return
	}

	vars := mux.Vars(r)

	startTime := time.Now().UnixNano()
	defer func() {
		endTime := time.Now().UnixNano()
		s.Stats.ResponsePostTime.AddPoint(float64(endTime - startTime))
	}()

	kafka := &KafkaParameters{
		Topic:     vars["topic"],
		Partition: toInt32(vars["partition"]),
		Offset:    -1,
	}

	msg, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Unable to read body: %s", err)
		return
	}

	var m json.RawMessage
	if err = json.Unmarshal(msg, &m); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Message must be JSON")
		return
	}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	parts, err := meta.Partitions(kafka.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	if !inSlice(kafka.Partition, parts) {
		s.errorResponse(w, http.StatusBadRequest, "Partition not found")
		return
	}

	producer, err := s.Client.NewProducer(s.Cfg)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to make producer: %v", err)
		return
	}
	defer producer.Close()

	kafka.Offset, err = producer.SendMessage(kafka.Topic, kafka.Partition, msg)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Unable to store your data: %v", err)
		return
	}

	s.successResponse(w, kafka)
}

func (s *Server) GetHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	if s.Cfg.Global.MaxConns > 0 && cl.Conns >= s.Cfg.Global.MaxConns {
		s.errorResponse(w, http.StatusServiceUnavailable, "Too many connections")
		return
	}

	vars := mux.Vars(r)

	startTime := time.Now().UnixNano()
	defer func() {
		endTime := time.Now().UnixNano()
		s.Stats.ResponseGetTime.AddPoint(float64(endTime - startTime))
	}()

	var (
		varsLength string
		varsOffset string
	)

	if varsLength = r.FormValue("limit"); varsLength == "" {
		varsLength = "1"
	}

	varsOffset = r.FormValue("offset")

	o := &ResponseMessages{
		Query: KafkaParameters{
			Topic:     vars["topic"],
			Partition: toInt32(vars["partition"]),
			Offset:    -1,
		},
		Messages: []json.RawMessage{},
	}

	length := toInt64(varsLength)

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	parts, err := meta.Partitions(o.Query.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	if !inSlice(o.Query.Partition, parts) {
		s.errorResponse(w, http.StatusBadRequest, "Partition not found")
		return
	}

	offsetFrom, err := meta.GetOffsetInfo(o.Query.Topic, o.Query.Partition, KafkaOffsetOldest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get offset: %v", err)
		return
	}

	if varsOffset == "" {
		// Set default value
		o.Query.Offset = offsetFrom
	} else {
		o.Query.Offset = toInt64(varsOffset)
	}

	offsetTo, err := meta.GetOffsetInfo(o.Query.Topic, o.Query.Partition, KafkaOffsetNewest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get offset: %v", err)
		return
	}

	offsetTo--

	if o.Query.Offset == 0 && offsetTo == 0 {
		// Topic is empty
		s.successResponse(w, o)
		return
	}

	if o.Query.Offset < offsetFrom || o.Query.Offset > offsetTo {
		s.errorResponse(w, http.StatusRequestedRangeNotSatisfiable,
			"Offset out of range (%v, %v)", offsetFrom, offsetTo)
		return
	}

	consumer, err := s.Client.NewConsumer(s.Cfg, o.Query.Topic, o.Query.Partition, o.Query.Offset)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to make consumer: %v", err)
		return
	}
	defer consumer.Close()

	for {
		msg, err := consumer.Message()
		if err != nil {
			if err == KafkaErrNoData {
				break
			}
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get message: %v", err)
			return
		}

		var m json.RawMessage

		if err := json.Unmarshal(msg.Value, &m); err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Bad JSON: %v", err)
			return
		}
		o.Messages = append(o.Messages, m)
		length--

		if msg.Offset >= offsetTo || length == 0 {
			break
		}
	}

	s.successResponse(w, o)
}

func (s *Server) GetTopicListHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	if s.Cfg.Global.MaxConns > 0 && cl.Conns >= s.Cfg.Global.MaxConns {
		s.errorResponse(w, http.StatusServiceUnavailable, "Too many connections")
		return
	}

	res := []ResponseTopicListInfo{}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	topics, err := meta.Topics()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get topics: %v", err)
		return
	}

	for _, topic := range topics {
		parts, err := meta.Partitions(topic)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
			return
		}
		info := &ResponseTopicListInfo{
			Topic:      topic,
			Partitions: len(parts),
		}
		res = append(res, *info)
	}

	s.successResponse(w, res)
}

func (s *Server) GetPartitionInfoHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	if s.Cfg.Global.MaxConns > 0 && cl.Conns >= s.Cfg.Global.MaxConns {
		s.errorResponse(w, http.StatusServiceUnavailable, "Too many connections")
		return
	}

	vars := mux.Vars(r)
	res := &ResponsePartitionInfo{
		Topic:     vars["topic"],
		Partition: toInt32(vars["partition"]),
	}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	res.Leader, err = meta.Leader(res.Topic, res.Partition)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get broker: %v", err)
		return
	}

	res.Replicas, err = meta.Replicas(res.Topic, res.Partition)
	if err != nil {
		if err != KafkaErrReplicaNotAvailable {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get replicas: %v", err)
			return
		}
		log.Printf("Error: Unable to get replicas: %v\n", err)
		res.Replicas = make([]int32, 0)
	}
	res.ReplicasNum = len(res.Replicas)

	res.OffsetNewest, err = meta.GetOffsetInfo(res.Topic, res.Partition, KafkaOffsetNewest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get newest offset: %v", err)
		return
	}

	res.OffsetOldest, err = meta.GetOffsetInfo(res.Topic, res.Partition, KafkaOffsetOldest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get oldest offset: %v", err)
		return
	}

	wp, err := meta.WritablePartitions(res.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get writable partitions: %v", err)
		return
	}

	res.Writable = inSlice(res.Partition, wp)

	s.successResponse(w, res)
}

func (s *Server) GetTopicInfoHandler(w http.ResponseWriter, r *http.Request) {
	cl := s.newConnTrack(r)
	defer s.closeConnTrack(cl)

	if s.Cfg.Global.MaxConns > 0 && cl.Conns >= s.Cfg.Global.MaxConns {
		s.errorResponse(w, http.StatusServiceUnavailable, "Too many connections")
		return
	}

	vars := mux.Vars(r)

	res := []ResponsePartitionInfo{}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	writable, err := meta.WritablePartitions(vars["topic"])
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get writable partitions: %v", err)
		return
	}

	parts, err := meta.Partitions(vars["topic"])
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	for partition := range parts {
		r := &ResponsePartitionInfo{
			Topic:     vars["topic"],
			Partition: int32(partition),
			Writable:  inSlice(int32(partition), writable),
		}

		r.Leader, err = meta.Leader(r.Topic, r.Partition)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get broker: %v", err)
			return
		}

		r.Replicas, err = meta.Replicas(r.Topic, r.Partition)
		if err != nil {
			if err != KafkaErrReplicaNotAvailable {
				s.errorResponse(w, http.StatusInternalServerError, "Unable to get replicas: %v", err)
				return
			}
			log.Printf("Error: Unable to get replicas: %v\n", err)
			r.Replicas = make([]int32, 0)
		}
		r.ReplicasNum = len(r.Replicas)

		r.OffsetNewest, err = meta.GetOffsetInfo(r.Topic, r.Partition, KafkaOffsetNewest)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get newest offset: %v", err)
			return
		}

		r.OffsetOldest, err = meta.GetOffsetInfo(r.Topic, r.Partition, KafkaOffsetOldest)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get oldest offset: %v", err)
			return
		}

		res = append(res, *r)
	}

	s.successResponse(w, res)
}

func (s *Server) fetchMetadata() (*KafkaMetadata, error) {
	s.Cache.Lock()
	defer s.Cache.Unlock()

	now := time.Now().UnixNano()

	if s.Cfg.Metadata.CacheTimeout.Duration > 0 {
		if (now - s.Cache.lastUpdateMetadata) < int64(s.Cfg.Metadata.CacheTimeout.Duration) {
			return s.Cache.lastMetadata, nil
		}
	}

	meta, err := s.Client.GetMetadata()
	if err != nil {
		return nil, err
	}

	s.Cache.lastUpdateMetadata = now
	s.Cache.lastMetadata = meta

	return meta, nil
}

func (s *Server) InitStatistics() {
	hmetrics2.SetPeriod(10 * time.Second)
	hmetrics2.AddHook(expvarexport.Exporter("Kafka"))

	s.Stats.ResponsePostTime = hmetrics2.NewHistogram()
	s.Stats.ResponseGetTime = hmetrics2.NewHistogram()

	hmetrics2.MustRegisterPackageMetric("Response.POST.Time", s.Stats.ResponsePostTime)
	hmetrics2.MustRegisterPackageMetric("Response.GET.Time", s.Stats.ResponseGetTime)

	s.Stats.HTTPStatus = make(map[int]*hmetrics2.Counter)
	s.Stats.HTTPStatus[200] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[400] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[404] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[416] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[500] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[502] = hmetrics2.NewCounter()
	s.Stats.HTTPStatus[503] = hmetrics2.NewCounter()

	for code := range s.Stats.HTTPStatus {
		hmetrics2.MustRegisterPackageMetric(fmt.Sprintf("Http.Status.%d", code), s.Stats.HTTPStatus[code])
	}

	type RuntimeStat struct {
		Goroutines      int
		CgoCall         int64
		CPU             int
		GoMaxProcs      int
		UsedDescriptors int
	}

	expvar.Publish("runtime", expvar.Func(func() interface{} {
		data := &RuntimeStat{
			Goroutines:      runtime.NumGoroutine(),
			CgoCall:         runtime.NumCgoCall(),
			CPU:             runtime.NumCPU(),
			GoMaxProcs:      runtime.GOMAXPROCS(0),
			UsedDescriptors: 0,
		}

		var nofileLimit syscall.Rlimit
		err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofileLimit)
		if err != nil {
			return data
		}

		for i := 0; i < int(nofileLimit.Cur); i++ {
			_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(i), syscall.F_GETFD, 0)
			if errno == 0 {
				data.UsedDescriptors++
			}
		}

		return data
	}))
}

func (s *Server) Run() error {
	r := mux.NewRouter()
	r.NotFoundHandler = http.HandlerFunc(s.NotFoundHandler)

	r.HandleFunc("/v1/topics/{topic:[A-Za-z0-9_-]+}/{partition:[0-9]+}", s.SendHandler).
		Methods("POST")

	r.HandleFunc("/v1/topics/{topic:[A-Za-z0-9_-]+}/{partition:[0-9]+}", s.GetHandler).
		Methods("GET")

	r.HandleFunc("/v1/info/topics/{topic:[A-Za-z0-9_-]+}/{partition:[0-9]+}", s.GetPartitionInfoHandler).
		Methods("GET")

	r.HandleFunc("/v1/info/topics/{topic:[A-Za-z0-9_-]+}", s.GetTopicInfoHandler).
		Methods("GET")

	r.HandleFunc("/v1/info/topics", s.GetTopicListHandler).
		Methods("GET")

	r.HandleFunc("/", s.RootHandler).
		Methods("GET")

	r.HandleFunc("/ping", s.PingHandler).
		Methods("GET")

	r.Handle("/debug/vars", http.DefaultServeMux)

	httpServer := &http.Server{
		Addr:    s.Cfg.Global.Address,
		Handler: handlers.LoggingHandler(s.Logfile, r),
	}

	if s.Cfg.Global.Verbose {
		log.Println("Server ready")
	}
	return httpServer.ListenAndServe()
}

func inSlice(n int32, list []int32) bool {
	for i := range list {
		if n == int32(i) {
			return true
		}
	}
	return false
}

func toInt32(s string) int32 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0
	}
	return int32(i)
}

func toInt64(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return i
}

func main() {
	log.SetPrefix("[server] ")

	flag.Parse()

	server := &Server{}
	defer func() {
		if err := server.Close(); err != nil {
			log.Println("Failed to close server", err)
		}
	}()
	server.Cfg.SetDefaults()

	if *config != "" {
		err := gcfg.ReadFileInto(&server.Cfg, *config)
		if err != nil {
			log.Fatal("Unable to read config file: ", err.Error())
		}
	}

	if *verbose {
		server.Cfg.Global.Verbose = true
	}

	if *brokers != "" {
		server.Cfg.Kafka.Broker = strings.Split(*brokers, ",")
	}

	if *addr != "" {
		server.Cfg.Global.Address = *addr
	}

	var err error

	server.Pidfile, err = OpenPidfile(server.Cfg.Global.Pidfile)
	if err != nil {
		log.Fatal("Unable to open pidfile: ", err.Error())
		return
	}
	defer server.Pidfile.Close()

	if err = server.Pidfile.Check(); err != nil {
		log.Fatal("Check failed: ", err.Error())
		os.Exit(1)
	}

	if err = server.Pidfile.Write(); err != nil {
		log.Fatal("Unable to write pidfile: ", err.Error())
		os.Exit(1)
	}

	server.Logfile, err = OpenLogfile(server.Cfg.Global.Logfile)
	if err != nil {
		log.Fatal("Unable to open log: ", err.Error())
		return
	}
	defer server.Logfile.Close()

	// Setup global log
	log.SetOutput(server.Logfile)

	if server.Cfg.Global.Address == "" {
		log.Println("Address required")
		os.Exit(1)
	}

	if len(server.Cfg.Kafka.Broker) == 0 {
		log.Println("Kafka brokers required")
		os.Exit(1)
	}

	if server.Cfg.Global.GoMaxProcs == 0 {
		server.Cfg.Global.GoMaxProcs = runtime.NumCPU()
	}
	runtime.GOMAXPROCS(server.Cfg.Global.GoMaxProcs)

	server.Cfg.Logfile = server.Logfile

	server.Client, err = NewClient(server.Cfg)
	if err != nil {
		log.Fatal("Unable to make client: ", err.Error())
		os.Exit(1)
	}
	defer server.Client.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	go func() {
		for {
			_ = <-sigChan
			if err := server.Logfile.Reopen(); err != nil {
				panic("Unable to reopen logfile")
			}
		}
	}()

	server.InitStatistics()

	log.Fatal(server.Run())
}
