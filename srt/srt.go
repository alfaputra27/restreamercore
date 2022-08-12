package srt

import (
	"container/ring"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/datarhei/core/v16/cluster"
	"github.com/datarhei/core/v16/log"
	"github.com/datarhei/core/v16/session"
	srt "github.com/datarhei/gosrt"
)

// ErrServerClosed is returned by ListenAndServe if the server
// has been closed regularly with the Close() function.
var ErrServerClosed = srt.ErrServerClosed

type client struct {
	conn      srt.Conn
	id        string
	createdAt time.Time

	txbytes uint64
	rxbytes uint64

	collector session.Collector

	cancel context.CancelFunc
}

func newClient(conn srt.Conn, id string, collector session.Collector) *client {
	c := &client{
		conn:      conn,
		id:        id,
		createdAt: time.Now(),

		collector: collector,
	}

	var ctx context.Context
	ctx, c.cancel = context.WithCancel(context.Background())

	go c.ticker(ctx)

	return c
}

func (c *client) ticker(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := c.conn.Stats()

			rxbytes := stats.ByteRecv
			txbytes := stats.ByteSent

			c.collector.Ingress(c.id, int64(rxbytes-c.rxbytes))
			c.collector.Egress(c.id, int64(txbytes-c.txbytes))

			c.txbytes = txbytes
			c.rxbytes = rxbytes
		}
	}
}

func (c *client) Close() {
	c.cancel()
}

// channel represents a stream that is sent to the server
type channel struct {
	pubsub    srt.PubSub
	collector session.Collector
	path      string

	publisher  *client
	subscriber map[string]*client
	lock       sync.RWMutex
}

func newChannel(conn srt.Conn, resource string, collector session.Collector) *channel {
	ch := &channel{
		pubsub:     srt.NewPubSub(srt.PubSubConfig{}),
		path:       resource,
		publisher:  newClient(conn, resource, collector),
		subscriber: make(map[string]*client),
		collector:  collector,
	}

	addr := conn.RemoteAddr().String()
	ip, _, _ := net.SplitHostPort(addr)

	if collector.IsCollectableIP(ip) {
		collector.RegisterAndActivate(resource, resource, "publish:"+resource, addr)
	}

	return ch
}

func (ch *channel) Close() {
	if ch.publisher == nil {
		return
	}

	ch.publisher.Close()
	ch.publisher = nil
}

func (ch *channel) AddSubscriber(conn srt.Conn, resource string) string {
	addr := conn.RemoteAddr().String()
	ip, _, _ := net.SplitHostPort(addr)

	client := newClient(conn, addr, ch.collector)

	if ch.collector.IsCollectableIP(ip) {
		ch.collector.RegisterAndActivate(addr, resource, "play:"+resource, addr)
	}

	ch.lock.Lock()
	ch.subscriber[addr] = client
	ch.lock.Unlock()

	return addr
}

func (ch *channel) RemoveSubscriber(id string) {
	ch.lock.Lock()
	defer ch.lock.Unlock()

	client := ch.subscriber[id]
	if client != nil {
		delete(ch.subscriber, id)
		client.Close()
	}
}

// Config for a new SRT server
type Config struct {
	// The address the SRT server should listen on, e.g. ":1935"
	Addr string

	// A token that needs to be added to the URL as query string
	// in order push or pull a stream. The key for the query
	// parameter is "token". Optional. By default no token is
	// required.
	Token string

	Passphrase string

	// Logger. Optional.
	Logger log.Logger

	Collector session.Collector

	SRTLogTopics []string

	Cluster cluster.ClusterReader
}

// Server represents a SRT server
type Server interface {
	// ListenAndServe starts the SRT server
	ListenAndServe() error

	// Close stops the RTMP server and closes all connections
	Close()

	// Channels return a list of currently publishing streams
	Channels() []Channel
}

// server implements the Server interface
type server struct {
	addr       string
	token      string
	passphrase string

	collector session.Collector

	server srt.Server

	// Map of publishing channels and a lock to serialize access to the map. The map
	// index is the name of the resource.
	channels map[string]*channel
	proxy    map[string]*proxy
	lock     sync.RWMutex

	logger log.Logger

	srtlogger       srt.Logger
	srtloggerCancel context.CancelFunc
	srtlog          map[string]*ring.Ring // Per logtopic a dedicated ring buffer
	srtlogLock      sync.RWMutex

	cluster cluster.ClusterReader
}

func New(config Config) (Server, error) {
	s := &server{
		addr:       config.Addr,
		token:      config.Token,
		passphrase: config.Passphrase,
		collector:  config.Collector,
		logger:     config.Logger,
		cluster:    config.Cluster,
	}

	if s.collector == nil {
		s.collector = session.NewNullCollector()
	}

	if s.cluster == nil {
		s.cluster = cluster.NewDummyClusterReader()
	}

	if s.logger == nil {
		s.logger = log.New("")
	}

	s.srtlogger = srt.NewLogger(config.SRTLogTopics)

	s.srtlogLock.Lock()
	s.srtlog = make(map[string]*ring.Ring)
	s.srtlogLock.Unlock()

	s.channels = make(map[string]*channel)
	s.proxy = make(map[string]*proxy)

	srtconfig := srt.DefaultConfig()

	srtconfig.KMPreAnnounce = 200
	srtconfig.KMRefreshRate = 10000
	srtconfig.Passphrase = config.Passphrase
	srtconfig.Logger = s.srtlogger

	s.server.Addr = config.Addr
	s.server.Config = &srtconfig
	s.server.HandleConnect = s.handleConnect
	s.server.HandlePublish = s.handlePublish
	s.server.HandleSubscribe = s.handleSubscribe

	return s, nil
}

func (s *server) ListenAndServe() error {
	var ctx context.Context
	ctx, s.srtloggerCancel = context.WithCancel(context.Background())

	go s.srtlogListener(ctx)

	return s.server.ListenAndServe()
}

func (s *server) Close() {
	s.server.Shutdown()

	s.srtloggerCancel()
}

type Log struct {
	Timestamp time.Time
	Message   []string
}

type Connection struct {
	Log   map[string][]Log
	Stats srt.Statistics
}

type Channel struct {
	Name        string                // Resource
	SocketId    uint32                // Socketid
	Subscriber  []uint32              // List of subscribed sockedids
	Connections map[uint32]Connection // Map from socketid to connection
	Log         map[string][]Log      // Map of topic to log entries
}

func (s *server) Channels() []Channel {
	channels := []Channel{}

	socket2channel := map[uint32]int{}

	s.lock.RLock()
	for id, ch := range s.channels {
		socketId := ch.publisher.conn.SocketId()
		channel := Channel{
			Name:        id,
			SocketId:    socketId,
			Subscriber:  []uint32{},
			Connections: map[uint32]Connection{},
			Log:         map[string][]Log{},
		}

		socket2channel[socketId] = len(channels)

		channel.Connections[socketId] = Connection{
			Stats: ch.publisher.conn.Stats(),
			Log:   map[string][]Log{},
		}

		for _, c := range ch.subscriber {
			socketId := c.conn.SocketId()
			channel.Subscriber = append(channel.Subscriber, socketId)

			channel.Connections[socketId] = Connection{
				Stats: c.conn.Stats(),
				Log:   map[string][]Log{},
			}

			socket2channel[socketId] = len(channels)
		}

		channels = append(channels, channel)
	}
	s.lock.RUnlock()

	s.srtlogLock.RLock()
	for topic, buf := range s.srtlog {
		buf.Do(func(l interface{}) {
			if l == nil {
				return
			}

			ll := l.(srt.Log)

			log := Log{
				Timestamp: ll.Time,
				Message:   strings.Split(ll.Message, "\n"),
			}

			if ll.SocketId == 0 {
				return
			}

			ch, ok := socket2channel[ll.SocketId]
			if !ok {
				return
			}

			channel := channels[ch]

			conn, ok := channel.Connections[ll.SocketId]
			if !ok {
				return
			}

			conn.Log[topic] = append(conn.Log[topic], log)

			channel.Connections[ll.SocketId] = conn

			channels[ch] = channel
		})
	}
	s.srtlogLock.RUnlock()

	return channels
}

func (s *server) srtlogListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-s.srtlogger.Listen():
			s.srtlogLock.Lock()
			if buf := s.srtlog[l.Topic]; buf == nil {
				s.srtlog[l.Topic] = ring.New(100)
			}
			s.srtlog[l.Topic].Value = l
			s.srtlog[l.Topic] = s.srtlog[l.Topic].Next()
			s.srtlogLock.Unlock()
		}
	}
}

func (s *server) log(handler, action, resource, message string, client net.Addr) {
	s.logger.Info().WithFields(log.Fields{
		"handler":  handler,
		"status":   action,
		"resource": resource,
		"client":   client.String(),
	}).Log(message)
}

type streamInfo struct {
	mode     string
	resource string
	token    string
}

// parseStreamId parses a streamid of the form "#!:key=value,key=value,..." and
// returns a streamInfo. In case the stream couldn't be parsed, an error is returned.
func parseStreamId(streamid string) (streamInfo, error) {
	si := streamInfo{}

	if !strings.HasPrefix(streamid, "#!:") {
		return si, fmt.Errorf("unknown streamid format")
	}

	streamid = strings.TrimPrefix(streamid, "#!:")

	kvs := strings.Split(streamid, ",")

	split := func(s, sep string) (string, string, error) {
		splitted := strings.SplitN(s, sep, 2)

		if len(splitted) != 2 {
			return "", "", fmt.Errorf("invalid key/value pair")
		}

		return splitted[0], splitted[1], nil
	}

	for _, kv := range kvs {
		key, value, err := split(kv, "=")
		if err != nil {
			continue
		}

		switch key {
		case "m":
			si.mode = value
		case "r":
			si.resource = value
		case "token":
			si.token = value
		default:
		}
	}

	return si, nil
}

func (s *server) handleConnect(req srt.ConnRequest) srt.ConnType {
	mode := srt.REJECT
	client := req.RemoteAddr()
	streamId := req.StreamId()

	si, err := parseStreamId(streamId)
	if err != nil {
		s.log("CONNECT", "INVALID", "", err.Error(), client)
		return srt.REJECT
	}

	if len(si.resource) == 0 {
		s.log("CONNECT", "INVALID", "", "stream resource not provided", client)
		return srt.REJECT
	}

	if si.mode == "publish" {
		mode = srt.PUBLISH
	} else if si.mode == "request" {
		mode = srt.SUBSCRIBE
	} else {
		s.log("CONNECT", "INVALID", si.resource, "invalid connection mode", client)
		return srt.REJECT
	}

	if len(s.passphrase) != 0 {
		if !req.IsEncrypted() {
			s.log("CONNECT", "FORBIDDEN", si.resource, "connection has to be encrypted", client)
			return srt.REJECT
		}

		if err := req.SetPassphrase(s.passphrase); err != nil {
			s.log("CONNECT", "FORBIDDEN", si.resource, err.Error(), client)
			return srt.REJECT
		}
	} else {
		if req.IsEncrypted() {
			s.log("CONNECT", "INVALID", si.resource, "connection must not be encrypted", client)
			return srt.REJECT
		}
	}

	// Check the token
	if len(s.token) != 0 && s.token != si.token {
		s.log("CONNECT", "FORBIDDEN", si.resource, "invalid token ("+si.token+")", client)
		return srt.REJECT
	}

	return mode
}

func (s *server) handlePublish(conn srt.Conn) {
	streamId := conn.StreamId()
	client := conn.RemoteAddr()

	si, _ := parseStreamId(streamId)

	// Look for the stream
	s.lock.Lock()
	ch := s.channels[si.resource]
	if ch == nil {
		ch = newChannel(conn, si.resource, s.collector)
		s.channels[si.resource] = ch
	} else {
		ch = nil
	}
	s.lock.Unlock()

	if ch == nil {
		s.log("PUBLISH", "CONFLICT", si.resource, "already publishing", client)
		conn.Close()
		return
	}

	s.log("PUBLISH", "START", si.resource, "", client)

	// Blocks until connection closes
	ch.pubsub.Publish(conn)

	s.lock.Lock()
	delete(s.channels, si.resource)
	s.lock.Unlock()

	ch.Close()

	s.log("PUBLISH", "STOP", si.resource, "", client)

	conn.Close()
}

type proxy struct {
	listeners uint64
	pubsub    srt.PubSub
	publisher srt.Conn
}

func (s *server) handleSubscribe(conn srt.Conn) {
	defer conn.Close()

	streamId := conn.StreamId()
	client := conn.RemoteAddr()

	si, _ := parseStreamId(streamId)

	// Look for the stream locally
	s.lock.RLock()
	ch := s.channels[si.resource]
	s.lock.RUnlock()

	if ch != nil {
		s.log("SUBSCRIBE", "START", si.resource, "", client)

		id := ch.AddSubscriber(conn, si.resource)

		// Blocks until connection closes
		ch.pubsub.Subscribe(conn)

		s.log("SUBSCRIBE", "STOP", si.resource, "", client)

		ch.RemoveSubscriber(id)

		return
	}

	// Check if the stream is already proxied
	s.lock.Lock()
	p := s.proxy[si.resource]

	if p == nil {
		// Check in the cluster for the stream and proxy it
		srturl, err := s.cluster.GetURL("srt:" + si.resource)
		if err != nil {
			s.log("SUBSCRIBE", "NOTFOUND", si.resource, "no publisher for this resource found", client)
			return
		}

		config := srt.DefaultConfig()
		config.Latency = 200 * time.Millisecond // This might be a value obtained from the cluster
		host, err := config.UnmarshalURL(srturl)
		if err != nil {
			s.logger.Error().WithField("address", srturl).WithError(err).Log("Parsing proxy address failed")
			s.log("SUBSCRIBE", "NOTFOUND", si.resource, "no publisher for this resource found", client)
			return
		}
		src, err := srt.Dial("srt", host, config)
		if err != nil {
			s.logger.Error().WithField("address", srturl).WithError(err).Log("Proxying address failed")
			s.log("SUBSCRIBE", "NOTFOUND", si.resource, "no publisher for this resource found", client)
			return
		}

		s.log("SUBSCRIBE", "PROXYPUBLISHSTART", srturl, "", client)

		p = &proxy{
			pubsub:    srt.NewPubSub(srt.PubSubConfig{}),
			publisher: src,
		}

		go func(resource string, p *proxy) {
			err := p.pubsub.Publish(p.publisher)
			if err != io.EOF {
				s.logger.Error().WithField("address", srturl).WithError(err).Log("Publish failed")
			}

			p.publisher.Close()

			s.log("SUBSCRIBE", "PROXYPUBLISHSTOP", srturl, "", client)

			s.lock.Lock()
			delete(s.proxy, resource)
			s.lock.Unlock()
		}(si.resource, p)

		s.proxy[si.resource] = p
	}
	s.lock.Unlock()

	if p != nil {
		p.listeners++

		s.log("SUBSCRIBE", "PROXYSTART", conn.StreamId(), "", client)

		// Blocks until connection closes
		err := p.pubsub.Subscribe(conn)
		if err != io.EOF {
			s.logger.Error().WithField("streamid", conn.StreamId()).WithError(err).Log("Subscribe failed")
		}

		s.log("SUBSCRIBE", "PROXYSTOP", conn.StreamId(), "", client)

		p.listeners--

		if p.listeners <= 0 {
			p.publisher.Close()
		}
	}
}
