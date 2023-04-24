package cluster

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	gonet "net"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/datarhei/core/v16/log"
	"github.com/datarhei/core/v16/net"
	"github.com/datarhei/core/v16/restream/app"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

/*
	/api/v3:
		GET /cluster/db/node - list all nodes that are stored in the FSM - Cluster.Store.ListNodes()
		POST /cluster/db/node - add a node to the FSM - Cluster.Store.AddNode()
		DELETE /cluster/db/node/:id - remove a node from the FSM - Cluster.Store.RemoveNode()

		GET /cluster/db/process - list all process configs that are stored in the FSM - Cluster.Store.ListProcesses()
		POST /cluster/db/process - add a process config to the FSM - Cluster.Store.AddProcess()
		PUT /cluster/db/process/:id - update a process config in the FSM - Cluster.Store.UpdateProcess()
		DELETE /cluster/db/process/:id - remove a process config from the FSM - Cluster.Store.RemoveProcess()

		** for the processes, the leader will decide where to actually run them. the process configs will
		also be added to the regular process DB of each core.

		POST /cluster/join - join the cluster - Cluster.Join()
		DELETE /cluster/:id - leave the cluster - Cluster.Leave()

		** all these endpoints will forward the request to the leader.
*/

var ErrNodeNotFound = errors.New("node not found")

type ClusterReader interface {
	GetURL(path string) (string, error)
	GetFile(path string) (io.ReadCloser, error)
}

type dummyClusterReader struct{}

func NewDummyClusterReader() ClusterReader {
	return &dummyClusterReader{}
}

func (r *dummyClusterReader) GetURL(path string) (string, error) {
	return "", fmt.Errorf("not implemented in dummy cluster")
}

func (r *dummyClusterReader) GetFile(path string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented in dummy cluster")
}

type Cluster interface {
	Addr() string
	APIAddr(raftAddress string) (string, error)

	Join(origin, id, raftAddress, apiAddress, apiUsername, apiPassword string) error
	Leave(origin, id string) error // gracefully remove a node from the cluster
	Snapshot() ([]byte, error)

	Shutdown() error

	AddNode(id, address, username, password string) error
	RemoveNode(id string) error
	ListNodes() []addNodeCommand
	GetNode(id string) (addNodeCommand, error)

	AddNodeX(address, username, password string) (string, error)
	RemoveNodeX(id string) error
	ListNodesX() []NodeReader
	GetNodeX(id string) (NodeReader, error)

	ClusterReader
}

type ClusterConfig struct {
	ID          string // ID of the node
	Name        string // Name of the node
	Path        string // Path where to store all cluster data
	Bootstrap   bool   // Whether to bootstrap a cluster
	Recover     bool   // Whether to recover this node
	Address     string // Listen address for the raft protocol
	JoinAddress string // Address of a member of a cluster to join

	CoreAPIAddress  string // Address of the core API
	CoreAPIUsername string // Username for the core API
	CoreAPIPassword string // Password for the core API

	IPLimiter net.IPLimiter
	Logger    log.Logger
}

type cluster struct {
	id   string
	name string
	path string

	nodes    map[string]*node     // List of known nodes
	idfiles  map[string][]string  // Map from nodeid to list of files
	idupdate map[string]time.Time // Map from nodeid to time of last update
	fileid   map[string]string    // Map from file name to nodeid

	limiter net.IPLimiter

	updates chan NodeState

	lock   sync.RWMutex
	cancel context.CancelFunc
	once   sync.Once

	logger log.Logger

	raft                  *raft.Raft
	raftTransport         *raft.NetworkTransport
	raftAddress           string
	raftNotifyCh          chan bool
	raftEmergencyNotifyCh chan bool
	raftStore             *raftboltdb.BoltStore
	raftRemoveGracePeriod time.Duration

	joinAddress string

	store Store

	reassertLeaderCh chan chan error

	leaveCh chan struct{}

	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex

	forwarder Forwarder
	api       API

	core struct {
		address  string
		username string
		password string
	}

	isRaftLeader  bool
	hasRaftLeader bool
	isLeader      bool
	leaderLock    sync.Mutex
}

func New(config ClusterConfig) (Cluster, error) {
	c := &cluster{
		id:       config.ID,
		name:     config.Name,
		path:     config.Path,
		nodes:    map[string]*node{},
		idfiles:  map[string][]string{},
		idupdate: map[string]time.Time{},
		fileid:   map[string]string{},
		limiter:  config.IPLimiter,
		updates:  make(chan NodeState, 64),
		logger:   config.Logger,

		raftAddress: config.Address,
		joinAddress: config.JoinAddress,

		reassertLeaderCh: make(chan chan error),
		leaveCh:          make(chan struct{}),
		shutdownCh:       make(chan struct{}),
	}

	c.core.address = config.CoreAPIAddress
	c.core.username = config.CoreAPIUsername
	c.core.password = config.CoreAPIPassword

	if c.limiter == nil {
		c.limiter = net.NewNullIPLimiter()
	}

	if c.logger == nil {
		c.logger = log.New("")
	}

	store, err := NewStore()
	if err != nil {
		return nil, err
	}

	c.store = store

	api, err := NewAPI(APIConfig{
		ID:      c.id,
		Cluster: c,
		Logger:  c.logger.WithField("logname", "api"),
	})
	if err != nil {
		return nil, err
	}

	go func(api API) {
		api.Start()
	}(api)

	c.api = api

	if forwarder, err := NewForwarder(ForwarderConfig{
		ID:     c.id,
		Logger: c.logger.WithField("logname", "forwarder"),
	}); err != nil {
		c.shutdownAPI()
		return nil, err
	} else {
		c.forwarder = forwarder
	}

	c.logger.Debug().Log("starting raft")

	err = c.startRaft(store, config.Bootstrap, config.Recover, false)
	if err != nil {
		c.shutdownAPI()
		return nil, err
	}

	if len(c.joinAddress) != 0 {
		addr, _ := c.APIAddr(c.joinAddress)
		c.forwarder.SetLeader(addr)

		go func(addr string) {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-c.shutdownCh:
					return
				case <-ticker.C:
					c.logger.Debug().Log("joining cluster at %s", c.joinAddress)
					err := c.Join("", c.id, c.raftAddress, c.core.address, c.core.username, c.core.password)
					if err != nil {
						c.logger.Warn().WithError(err).Log("unable to join %s", c.joinAddress)
						continue
					}

					return
				}
			}
		}(addr)
	}

	go func() {
		for {
			select {
			case <-c.shutdownCh:
				return
			case state := <-c.updates:
				c.logger.Debug().WithFields(log.Fields{
					"node":  state.ID,
					"state": state.State,
					"files": len(state.Files),
				}).Log("Got update")

				c.lock.Lock()

				// Cleanup
				files := c.idfiles[state.ID]
				for _, file := range files {
					delete(c.fileid, file)
				}
				delete(c.idfiles, state.ID)
				delete(c.idupdate, state.ID)

				if state.State == "connected" {
					// Add files
					for _, file := range state.Files {
						c.fileid[file] = state.ID
					}
					c.idfiles[state.ID] = files
					c.idupdate[state.ID] = state.LastUpdate
				}

				c.lock.Unlock()
			}
		}
	}()

	return c, nil
}

func (c *cluster) Addr() string {
	return c.raftAddress
}

func (c *cluster) APIAddr(raftAddress string) (string, error) {
	if len(raftAddress) == 0 {
		raftAddress = c.raftAddress
	}

	host, port, _ := gonet.SplitHostPort(raftAddress)

	p, err := strconv.Atoi(port)
	if err != nil {
		return "", err
	}

	return gonet.JoinHostPort(host, strconv.Itoa(p+1)), nil
}

func (c *cluster) Shutdown() error {
	c.logger.Info().Log("shutting down cluster")
	c.shutdownLock.Lock()
	defer c.shutdownLock.Unlock()

	if c.shutdown {
		return nil
	}

	c.shutdown = true
	close(c.shutdownCh)

	c.lock.Lock()
	defer c.lock.Unlock()

	for _, node := range c.nodes {
		node.stop()
	}

	c.nodes = map[string]*node{}

	c.shutdownRaft()

	return nil
}

func (c *cluster) IsRaftLeader() bool {
	c.leaderLock.Lock()
	defer c.leaderLock.Unlock()

	return c.isRaftLeader
}

func (c *cluster) Leave(origin, id string) error {
	if len(id) == 0 {
		id = c.id
	}

	c.logger.Debug().WithFields(log.Fields{
		"nodeid": id,
	}).Log("received leave request for node")

	if !c.IsRaftLeader() {
		// Tell the leader to remove us
		err := c.forwarder.Leave(origin, id)
		if err != nil {
			return err
		}

		// Wait for us being removed from the configuration
		left := false
		limit := time.Now().Add(c.raftRemoveGracePeriod)
		for !left && time.Now().Before(limit) {
			c.logger.Debug().Log("waiting for getting removed from the configuration")
			// Sleep a while before we check.
			time.Sleep(50 * time.Millisecond)

			// Get the latest configuration.
			future := c.raft.GetConfiguration()
			if err := future.Error(); err != nil {
				c.logger.Error().WithError(err).Log("failed to get raft configuration")
				break
			}

			// See if we are no longer included.
			left = true
			for _, server := range future.Configuration().Servers {
				if server.Address == raft.ServerAddress(c.raftAddress) {
					left = false
					break
				}
			}
		}

		if !left {
			c.logger.Warn().Log("failed to leave raft configuration gracefully, timeout")
		}

		return nil
	}

	// Count the number of servers in the cluster
	future := c.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		c.logger.Error().WithError(err).Log("failed to get raft configuration")
		return err
	}

	numPeers := len(future.Configuration().Servers)

	if id == c.id {
		// We're going to remove ourselves
		if numPeers <= 1 {
			// Don't do so if we're the only server in the cluster
			c.logger.Debug().Log("we're the leader without any peers, not doing anything")
			return nil
		}

		// Transfer the leadership to another server
		err := c.leadershipTransfer()
		if err != nil {
			c.logger.Warn().WithError(err).Log("failed to transfer leadership")
			return err
		}

		// Wait for new leader election
		for {
			c.logger.Debug().Log("waiting for new leader election")

			time.Sleep(50 * time.Millisecond)

			c.leaderLock.Lock()
			hasLeader := c.hasRaftLeader
			c.leaderLock.Unlock()

			if hasLeader {
				break
			}
		}

		// Tell the new leader to remove us
		err = c.forwarder.Leave("", id)
		if err != nil {
			return err
		}

		// Wait for us being removed from the configuration
		left := false
		limit := time.Now().Add(c.raftRemoveGracePeriod)
		for !left && time.Now().Before(limit) {
			c.logger.Debug().Log("waiting for getting removed from the configuration")
			// Sleep a while before we check.
			time.Sleep(50 * time.Millisecond)

			// Get the latest configuration.
			future := c.raft.GetConfiguration()
			if err := future.Error(); err != nil {
				c.logger.Error().WithError(err).Log("failed to get raft configuration")
				break
			}

			// See if we are no longer included.
			left = true
			for _, server := range future.Configuration().Servers {
				if server.Address == raft.ServerAddress(c.raftAddress) {
					left = false
					break
				}
			}
		}

		return nil
	}

	err := c.RemoveNode(id)
	if err != nil {
		c.logger.Error().WithError(err).Log("failed to apply log for removal")
	}

	// Remove another sever from the cluster
	if future := c.raft.RemoveServer(raft.ServerID(id), 0, 0); future.Error() != nil {
		c.logger.Error().WithError(future.Error()).WithFields(log.Fields{
			"nodeid": id,
		}).Log("failed to remove node")

		return future.Error()
	}

	return nil
}

func (c *cluster) Join(origin, id, raftAddress, apiAddress, apiUsername, apiPassword string) error {
	if !c.IsRaftLeader() {
		c.logger.Debug().Log("not leader, forwarding to leader")
		return c.forwarder.Join(origin, id, raftAddress, apiAddress, apiUsername, apiPassword)
	}

	c.logger.Debug().Log("leader: joining %s", raftAddress)

	c.logger.Debug().WithFields(log.Fields{
		"nodeid":  id,
		"address": raftAddress,
	}).Log("received join request for remote node")

	configFuture := c.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		c.logger.Error().WithError(err).Log("failed to get raft configuration")
		return err
	}

	for _, srv := range configFuture.Configuration().Servers {
		// If a node already exists with either the joining node's ID or address,
		// that node may need to be removed from the config first.
		if srv.ID == raft.ServerID(id) || srv.Address == raft.ServerAddress(raftAddress) {
			// However if *both* the ID and the address are the same, then nothing -- not even
			// a join operation -- is needed.
			if srv.ID == raft.ServerID(id) && srv.Address == raft.ServerAddress(raftAddress) {
				c.logger.Debug().WithFields(log.Fields{
					"nodeid":  id,
					"address": raftAddress,
				}).Log("node is already member of cluster, ignoring join request")
				return nil
			}

			future := c.raft.RemoveServer(srv.ID, 0, 0)
			if err := future.Error(); err != nil {
				c.logger.Error().WithError(err).WithFields(log.Fields{
					"nodeid":  id,
					"address": raftAddress,
				}).Log("error removing existing node")
				return fmt.Errorf("error removing existing node %s at %s: %w", id, raftAddress, err)
			}
		}
	}

	f := c.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(raftAddress), 0, 0)
	if err := f.Error(); err != nil {
		return err
	}

	if err := c.AddNode(id, apiAddress, apiUsername, apiPassword); err != nil {
		/*
			future := c.raft.RemoveServer(raft.ServerID(id), 0, 0)
			if err := future.Error(); err != nil {
				c.logger.Error().WithError(err).WithFields(log.Fields{
					"nodeid":  id,
					"address": raftAddress,
				}).Log("error removing existing node")
				return err
			}
			return err
		*/
		c.logger.Debug().WithError(err).Log("")
	}

	c.logger.Info().WithFields(log.Fields{
		"nodeid":  id,
		"address": raftAddress,
	}).Log("node joined successfully")

	return nil
}

type Snapshot struct {
	Metadata *raft.SnapshotMeta
	Data     []byte
}

func (c *cluster) Snapshot() ([]byte, error) {
	if !c.IsRaftLeader() {
		c.logger.Debug().Log("not leader, forwarding to leader")
		return c.forwarder.Snapshot()
	}

	f := c.raft.Snapshot()
	err := f.Error()
	if err != nil {
		return nil, err
	}

	metadata, r, err := f.Open()
	if err != nil {
		return nil, err
	}

	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read in snapshot: %w", err)
	}

	snapshot := Snapshot{
		Metadata: metadata,
		Data:     data,
	}

	buffer := bytes.Buffer{}
	enc := gob.NewEncoder(&buffer)
	err = enc.Encode(snapshot)
	if err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func (c *cluster) ListNodes() []addNodeCommand {
	c.store.ListNodes()

	return nil
}

func (c *cluster) GetNode(id string) (addNodeCommand, error) {
	c.store.GetNode(id)

	return addNodeCommand{}, nil
}

func (c *cluster) AddNode(id, address, username, password string) error {
	if !c.IsRaftLeader() {
		return fmt.Errorf("not leader")
	}

	com := &command{
		Operation: opAddNode,
		Data: &addNodeCommand{
			ID:       id,
			Address:  address,
			Username: username,
			Password: password,
		},
	}

	b, err := json.Marshal(com)
	if err != nil {
		return err
	}

	future := c.raft.Apply(b, 5*time.Second)
	if err := future.Error(); err != nil {
		return fmt.Errorf("applying command failed: %w", err)
	}

	return nil
}

func (c *cluster) RemoveNode(id string) error {
	if !c.IsRaftLeader() {
		return fmt.Errorf("not leader")
	}

	com := &command{
		Operation: opRemoveNode,
		Data: &removeNodeCommand{
			ID: id,
		},
	}

	b, err := json.Marshal(com)
	if err != nil {
		return err
	}

	future := c.raft.Apply(b, 5*time.Second)
	if err := future.Error(); err != nil {
		return fmt.Errorf("applying command failed: %w", err)
	}

	return nil
}

// trackLeaderChanges registers an Observer with raft in order to receive updates
// about leader changes, in order to keep the forwarder up to date.
func (c *cluster) trackLeaderChanges() {
	obsCh := make(chan raft.Observation, 16)
	observer := raft.NewObserver(obsCh, false, func(o *raft.Observation) bool {
		_, leaderOK := o.Data.(raft.LeaderObservation)
		_, peerOK := o.Data.(raft.PeerObservation)

		return leaderOK || peerOK
	})
	c.raft.RegisterObserver(observer)

	for {
		select {
		case obs := <-obsCh:
			if leaderObs, ok := obs.Data.(raft.LeaderObservation); ok {
				c.logger.Debug().WithFields(log.Fields{
					"id":      leaderObs.LeaderID,
					"address": leaderObs.LeaderAddr,
				}).Log("new leader observation")
				addr := string(leaderObs.LeaderAddr)
				if len(addr) != 0 {
					addr, _ = c.APIAddr(addr)
				}
				c.forwarder.SetLeader(addr)
				c.leaderLock.Lock()
				if len(addr) == 0 {
					c.hasRaftLeader = false
				} else {
					c.hasRaftLeader = true
				}
				c.leaderLock.Unlock()
			} else if peerObs, ok := obs.Data.(raft.PeerObservation); ok {
				c.logger.Debug().WithFields(log.Fields{
					"removed": peerObs.Removed,
					"address": peerObs.Peer.Address,
				}).Log("new peer observation")
			} else {
				c.logger.Debug().WithField("type", reflect.TypeOf(obs.Data)).Log("got unknown observation type from raft")
				continue
			}
		case <-c.shutdownCh:
			c.raft.DeregisterObserver(observer)
			return
		}
	}
}

func (c *cluster) AddNodeX(address, username, password string) (string, error) {
	node, err := newNode(address, username, password, c.updates)
	if err != nil {
		return "", err
	}

	id := node.ID()

	if id == c.id {
		return "", fmt.Errorf("can't add myself as node or a node with the same ID")
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	if _, ok := c.nodes[id]; ok {
		node.stop()
		return id, nil
	}

	ips := node.IPs()
	for _, ip := range ips {
		c.limiter.AddBlock(ip)
	}

	c.nodes[id] = node

	c.logger.Info().WithFields(log.Fields{
		"address": address,
		"id":      id,
	}).Log("Added node")

	return id, nil
}

func (c *cluster) RemoveNodeX(id string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	node, ok := c.nodes[id]
	if !ok {
		return ErrNodeNotFound
	}

	node.stop()

	delete(c.nodes, id)

	ips := node.IPs()

	for _, ip := range ips {
		c.limiter.RemoveBlock(ip)
	}

	c.Leave("", id)

	c.logger.Info().WithFields(log.Fields{
		"id": id,
	}).Log("Removed node")

	return nil
}

func (c *cluster) ListNodesX() []NodeReader {
	list := []NodeReader{}

	c.lock.RLock()
	defer c.lock.RUnlock()

	for _, node := range c.nodes {
		list = append(list, node)
	}

	return list
}

func (c *cluster) GetNodeX(id string) (NodeReader, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	node, ok := c.nodes[id]
	if !ok {
		return nil, fmt.Errorf("node not found")
	}

	return node, nil
}

func (c *cluster) GetURL(path string) (string, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	id, ok := c.fileid[path]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("Not found")
		return "", fmt.Errorf("file not found")
	}

	ts, ok := c.idupdate[id]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("No age information found")
		return "", fmt.Errorf("file not found")
	}

	if time.Since(ts) > 2*time.Second {
		c.logger.Debug().WithField("path", path).Log("File too old")
		return "", fmt.Errorf("file not found")
	}

	node, ok := c.nodes[id]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("Unknown node")
		return "", fmt.Errorf("file not found")
	}

	url, err := node.getURL(path)
	if err != nil {
		c.logger.Debug().WithField("path", path).Log("Invalid path")
		return "", fmt.Errorf("file not found")
	}

	c.logger.Debug().WithField("url", url).Log("File cluster url")

	return url, nil
}

func (c *cluster) GetFile(path string) (io.ReadCloser, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	id, ok := c.fileid[path]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("Not found")
		return nil, fmt.Errorf("file not found")
	}

	ts, ok := c.idupdate[id]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("No age information found")
		return nil, fmt.Errorf("file not found")
	}

	if time.Since(ts) > 2*time.Second {
		c.logger.Debug().WithField("path", path).Log("File too old")
		return nil, fmt.Errorf("file not found")
	}

	node, ok := c.nodes[id]
	if !ok {
		c.logger.Debug().WithField("path", path).Log("Unknown node")
		return nil, fmt.Errorf("file not found")
	}

	data, err := node.getFile(path)
	if err != nil {
		c.logger.Debug().WithField("path", path).Log("Invalid path")
		return nil, fmt.Errorf("file not found")
	}

	c.logger.Debug().WithField("path", path).Log("File cluster path")

	return data, nil
}

func (c *cluster) startRaft(fsm raft.FSM, bootstrap, recover, inmem bool) error {
	defer func() {
		if c.raft == nil && c.raftStore != nil {
			c.raftStore.Close()
		}
	}()

	c.raftRemoveGracePeriod = 5 * time.Second

	addr, err := gonet.ResolveTCPAddr("tcp", c.raftAddress)
	if err != nil {
		return err
	}

	c.logger.Debug().Log("address: %s", addr)

	transport, err := raft.NewTCPTransportWithLogger(c.raftAddress, addr, 3, 10*time.Second, NewLogger(c.logger, hclog.Debug).Named("raft-transport"))
	if err != nil {
		return err
	}

	c.raftTransport = transport

	snapshotLogger := NewLogger(c.logger, hclog.Debug).Named("raft-snapshot")
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(c.path, 3, snapshotLogger)
	if err != nil {
		return err
	}

	var logStore raft.LogStore
	var stableStore raft.StableStore
	if inmem {
		logStore = raft.NewInmemStore()
		stableStore = raft.NewInmemStore()
	} else {
		bolt, err := raftboltdb.New(raftboltdb.Options{
			Path: filepath.Join(c.path, "raftlog.db"),
			BoltOptions: &bbolt.Options{
				Timeout: 5 * time.Second,
			},
		})
		if err != nil {
			return fmt.Errorf("bolt: %w", err)
		}
		logStore = bolt
		stableStore = bolt

		cacheStore, err := raft.NewLogCache(512, logStore)
		if err != nil {
			return err
		}
		logStore = cacheStore

		c.raftStore = bolt
	}

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(c.id)
	cfg.Logger = NewLogger(c.logger, hclog.Debug).Named("raft")

	if bootstrap {
		hasState, err := raft.HasExistingState(logStore, stableStore, snapshots)
		if err != nil {
			return err
		}
		if !hasState {
			configuration := raft.Configuration{
				Servers: []raft.Server{
					{
						Suffrage: raft.Voter,
						ID:       raft.ServerID(c.id),
						Address:  transport.LocalAddr(),
					},
				},
			}

			if err := raft.BootstrapCluster(cfg, logStore, stableStore, snapshots, transport, configuration); err != nil {
				return err
			}
		}
	}

	// Set up a channel for reliable leader notifications.
	raftNotifyCh := make(chan bool, 10)
	cfg.NotifyCh = raftNotifyCh
	c.raftNotifyCh = raftNotifyCh

	c.raftEmergencyNotifyCh = make(chan bool, 10)

	node, err := raft.NewRaft(cfg, fsm, logStore, stableStore, snapshots, transport)
	if err != nil {
		return err
	}

	c.raft = node

	go c.trackLeaderChanges()
	go c.monitorLeadership()
	go c.sentinel()

	c.logger.Debug().Log("raft started")

	return nil
}

func (c *cluster) shutdownRaft() {
	if c.raft != nil {
		c.raftTransport.Close()
		future := c.raft.Shutdown()
		if err := future.Error(); err != nil {
			c.logger.Warn().WithError(err).Log("error shutting down raft")
		}
		if c.raftStore != nil {
			c.raftStore.Close()
		}
	}
}

func (c *cluster) shutdownAPI() {
	if c.api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		c.api.Shutdown(ctx)
	}
}

// nodeLoop is run by every node in the cluster. This is mainly to check the list
// of nodes from the FSM, in order to connect to them and to fetch their file lists.
func (c *cluster) followerLoop(stopCh chan struct{}) {
	// Periodically reconcile as long as we are the leader
	for {
		select {
		case <-stopCh:
			return
		case <-c.shutdownCh:
			return
		}
	}
}

func (c *cluster) AddProcess(config app.Config) error {
	if !c.IsRaftLeader() {
		return c.forwarder.AddProcess()
	}

	cmd := &command{
		Operation: "addProcess",
		Data: &addProcessCommand{
			Config: nil,
		},
	}

	return c.applyCommand(cmd)
}

func (c *cluster) applyCommand(cmd *command) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	future := c.raft.Apply(b, 5*time.Second)
	if err := future.Error(); err != nil {
		return fmt.Errorf("applying command failed: %w", err)
	}

	return nil
}

func (c *cluster) sentinel() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	start := time.Now()
	var lastContactSince time.Duration

	isEmergencyLeader := false

	for {
		select {
		case <-c.shutdownCh:
			return
		case <-ticker.C:
			stats := c.raft.Stats()

			fields := log.Fields{}

			for k, v := range stats {
				fields[k] = v
			}

			c.logger.Debug().WithFields(fields).Log("stats")

			lastContact := stats["last_contact"]
			if lastContact == "never" {
				lastContactSince = time.Since(start)
			} else {
				if d, err := time.ParseDuration(lastContact); err == nil {
					lastContactSince = d
					start = time.Now()
				} else {
					lastContactSince = time.Since(start)
				}
			}

			if lastContactSince > 10*time.Second && !isEmergencyLeader {
				c.logger.Warn().Log("force leadership due to lost contact to leader")
				c.raftEmergencyNotifyCh <- true
				isEmergencyLeader = true
			} else if lastContactSince <= 10*time.Second && isEmergencyLeader {
				c.logger.Warn().Log("stop forced leadership due to contact to leader")
				c.raftEmergencyNotifyCh <- false
				isEmergencyLeader = false
			}
		}
	}
}
