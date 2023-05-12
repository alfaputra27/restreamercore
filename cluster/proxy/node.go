package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	client "github.com/datarhei/core-client-go/v16"
	clientapi "github.com/datarhei/core-client-go/v16/api"
	"github.com/datarhei/core/v16/restream/app"
)

type Node interface {
	Connect() error
	Disconnect()

	StartFiles(updates chan<- NodeFiles) error
	StopFiles()

	GetURL(path string) (string, error)
	GetFile(path string) (io.ReadCloser, error)

	ProcessList() ([]Process, error)
	ProcessAdd(*app.Config) error
	ProcessStart(id string) error
	ProcessStop(id string) error
	ProcessDelete(id string) error

	NodeReader
}

type NodeReader interface {
	IPs() []string
	Files() NodeFiles
	About() NodeAbout
	Version() NodeVersion
}

type NodeFiles struct {
	ID         string
	Files      []string
	LastUpdate time.Time
}

type NodeResources struct {
	NCPU     float64 // Number of CPU on this node
	CPU      float64 // Current CPU load, 0-100*ncpu
	CPULimit float64 // Defined CPU load limit, 0-100*ncpu
	Mem      uint64  // Currently used memory in bytes
	MemLimit uint64  // Defined memory limit in bytes
}

type NodeAbout struct {
	ID          string
	Name        string
	Address     string
	State       string
	CreatedAt   time.Time
	Uptime      time.Duration
	LastContact time.Time
	Latency     time.Duration
	Resources   NodeResources
}

type NodeVersion struct {
	Number   string
	Commit   string
	Branch   string
	Build    time.Time
	Arch     string
	Compiler string
}

type nodeState string

func (n nodeState) String() string {
	return string(n)
}

const (
	stateDisconnected nodeState = "disconnected"
	stateConnected    nodeState = "connected"
)

type node struct {
	address string
	ips     []string

	peer       client.RestClient
	peerLock   sync.RWMutex
	cancelPing context.CancelFunc

	lastContact time.Time

	resources struct {
		ncpu     float64
		cpu      float64
		mem      uint64
		memTotal uint64
	}

	state       nodeState
	latency     float64 // Seconds
	stateLock   sync.RWMutex
	updates     chan<- NodeFiles
	filesList   []string
	lastUpdate  time.Time
	cancelFiles context.CancelFunc

	runningLock sync.Mutex
	running     bool

	host          string
	secure        bool
	hasRTMP       bool
	rtmpAddress   string
	rtmpToken     string
	hasSRT        bool
	srtAddress    string
	srtPassphrase string
	srtToken      string
}

func NewNode(address string) Node {
	n := &node{
		address: address,
		state:   stateDisconnected,
		secure:  strings.HasPrefix(address, "https://"),
	}

	return n
}

func (n *node) Connect() error {
	n.peerLock.Lock()
	defer n.peerLock.Unlock()

	if n.peer != nil {
		return nil
	}

	u, err := url.Parse(n.address)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("lookup failed: %w", err)
	}

	peer, err := client.New(client.Config{
		Address:    n.address,
		Auth0Token: "",
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	})
	if err != nil {
		return fmt.Errorf("creating client failed (%s): %w", n.address, err)
	}

	version, cfg, err := peer.Config()
	if err != nil {
		return err
	}

	if version != 3 {
		return fmt.Errorf("unsupported core config version: %d", version)
	}

	config, ok := cfg.Config.(clientapi.ConfigV3)
	if !ok {
		return fmt.Errorf("failed to convert config to expected version")
	}

	if config.RTMP.Enable {
		n.hasRTMP = true
		n.rtmpAddress = "rtmp://"

		isHostIP := net.ParseIP(host) != nil

		address := config.RTMP.Address
		if n.secure && config.RTMP.EnableTLS && !isHostIP {
			address = config.RTMP.AddressTLS
			n.rtmpAddress = "rtmps://"
		}

		_, port, err := net.SplitHostPort(address)
		if err != nil {
			n.hasRTMP = false
		} else {
			n.rtmpAddress += host + ":" + port
			n.rtmpToken = config.RTMP.Token
		}
	}

	if config.SRT.Enable {
		n.hasSRT = true
		n.srtAddress = "srt://"

		_, port, err := net.SplitHostPort(config.SRT.Address)
		if err != nil {
			n.hasSRT = false
		} else {
			n.srtAddress += host + ":" + port
			n.srtPassphrase = config.SRT.Passphrase
			n.srtToken = config.SRT.Token
		}
	}

	n.ips = addrs
	n.host = host

	n.peer = peer

	ctx, cancel := context.WithCancel(context.Background())
	n.cancelPing = cancel

	go func(ctx context.Context) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Ping
				ok, latency := n.peer.Ping()

				n.stateLock.Lock()
				if !ok {
					n.state = stateDisconnected
				} else {
					n.lastContact = time.Now()
					n.state = stateConnected
				}
				n.latency = n.latency*0.2 + latency.Seconds()*0.8
				n.stateLock.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}(ctx)

	go func(ctx context.Context) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Metrics
				metrics, err := n.peer.Metrics(clientapi.MetricsQuery{
					Metrics: []clientapi.MetricsQueryMetric{
						{
							Name: "cpu_ncpu",
						},
						{
							Name: "cpu_idle",
						},
						{
							Name: "mem_total",
						},
						{
							Name: "mem_free",
						},
					},
				})
				if err != nil {
					n.stateLock.Lock()
					n.resources.cpu = 100
					n.resources.ncpu = 1
					n.resources.mem = 0
					n.stateLock.Unlock()
				}

				cpu_ncpu := .0
				cpu_idle := .0
				mem_total := uint64(0)
				mem_free := uint64(0)

				for _, x := range metrics.Metrics {
					if x.Name == "cpu_idle" {
						cpu_idle = x.Values[0].Value
					} else if x.Name == "cpu_ncpu" {
						cpu_ncpu = x.Values[0].Value
					} else if x.Name == "mem_total" {
						mem_total = uint64(x.Values[0].Value)
					} else if x.Name == "mem_free" {
						mem_free = uint64(x.Values[0].Value)
					}
				}

				n.stateLock.Lock()
				n.resources.ncpu = cpu_ncpu
				n.resources.cpu = (100 - cpu_idle) * cpu_ncpu
				if mem_total != 0 {
					n.resources.mem = mem_total - mem_free
					n.resources.memTotal = mem_total
				} else {
					n.resources.mem = 0
					n.resources.memTotal = 0
				}
				n.lastContact = time.Now()
				n.stateLock.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}(ctx)

	return nil
}

func (n *node) Disconnect() {
	n.peerLock.Lock()
	defer n.peerLock.Unlock()

	if n.cancelPing != nil {
		n.cancelPing()
		n.cancelPing = nil
	}

	n.peer = nil
}

func (n *node) StartFiles(updates chan<- NodeFiles) error {
	n.runningLock.Lock()
	defer n.runningLock.Unlock()

	if n.running {
		return nil
	}

	n.running = true
	n.updates = updates

	ctx, cancel := context.WithCancel(context.Background())
	n.cancelFiles = cancel

	go func(ctx context.Context) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n.files()

				select {
				case n.updates <- n.Files():
				default:
				}
			}
		}
	}(ctx)

	return nil
}

func (n *node) StopFiles() {
	n.runningLock.Lock()
	defer n.runningLock.Unlock()

	if !n.running {
		return
	}

	n.running = false

	n.cancelFiles()
}

func (n *node) About() NodeAbout {
	n.peerLock.RLock()

	if n.peer == nil {
		n.peerLock.RUnlock()
		return NodeAbout{}
	}

	about := n.peer.About()

	n.peerLock.RUnlock()

	createdAt, err := time.Parse(time.RFC3339, about.CreatedAt)
	if err != nil {
		createdAt = time.Now()
	}

	n.stateLock.RLock()
	defer n.stateLock.RUnlock()

	state := NodeAbout{
		ID:          about.ID,
		Name:        about.Name,
		Address:     n.address,
		State:       n.state.String(),
		CreatedAt:   createdAt,
		Uptime:      time.Since(createdAt),
		LastContact: n.lastContact,
		Latency:     time.Duration(n.latency * float64(time.Second)),
		Resources: NodeResources{
			NCPU:     n.resources.ncpu,
			CPU:      n.resources.cpu,
			CPULimit: 90 * n.resources.ncpu,
			Mem:      n.resources.mem,
			MemLimit: uint64(float64(n.resources.memTotal) * 0.9),
		},
	}

	return state
}

func (n *node) Version() NodeVersion {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return NodeVersion{}
	}

	about := n.peer.About()

	build, err := time.Parse(time.RFC3339, about.Version.Build)
	if err != nil {
		build = time.Time{}
	}

	version := NodeVersion{
		Number:   about.Version.Number,
		Commit:   about.Version.Commit,
		Branch:   about.Version.Branch,
		Build:    build,
		Arch:     about.Version.Arch,
		Compiler: about.Version.Compiler,
	}

	return version
}

func (n *node) IPs() []string {
	return n.ips
}

func (n *node) Files() NodeFiles {
	n.stateLock.RLock()
	defer n.stateLock.RUnlock()

	state := NodeFiles{
		ID:         n.About().ID,
		LastUpdate: n.lastUpdate,
	}

	if n.state != stateDisconnected && time.Since(n.lastUpdate) <= 2*time.Second {
		state.Files = make([]string, len(n.filesList))
		copy(state.Files, n.filesList)
	}

	return state
}

func (n *node) files() {
	filesChan := make(chan string, 1024)
	filesList := []string{}

	wgList := sync.WaitGroup{}
	wgList.Add(1)

	go func() {
		defer wgList.Done()

		for file := range filesChan {
			if len(file) == 0 {
				return
			}

			filesList = append(filesList, file)
		}
	}()

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func(f chan<- string) {
		defer wg.Done()

		n.peerLock.RLock()
		defer n.peerLock.RUnlock()

		if n.peer == nil {
			return
		}

		files, err := n.peer.MemFSList("name", "asc")
		if err != nil {
			return
		}

		for _, file := range files {
			f <- "mem:" + file.Name
		}
	}(filesChan)

	go func(f chan<- string) {
		defer wg.Done()

		n.peerLock.RLock()
		defer n.peerLock.RUnlock()

		if n.peer == nil {
			return
		}

		files, err := n.peer.DiskFSList("name", "asc")
		if err != nil {
			return
		}

		for _, file := range files {
			f <- "disk:" + file.Name
		}
	}(filesChan)

	if n.hasRTMP {
		wg.Add(1)

		go func(f chan<- string) {
			defer wg.Done()

			n.peerLock.RLock()
			defer n.peerLock.RUnlock()

			if n.peer == nil {
				return
			}

			files, err := n.peer.RTMPChannels()
			if err != nil {
				return
			}

			for _, file := range files {
				f <- "rtmp:" + file.Name
			}
		}(filesChan)
	}

	if n.hasSRT {
		wg.Add(1)

		go func(f chan<- string) {
			defer wg.Done()

			n.peerLock.RLock()
			defer n.peerLock.RUnlock()

			if n.peer == nil {
				return
			}

			files, err := n.peer.SRTChannels()
			if err != nil {
				return
			}

			for _, file := range files {
				f <- "srt:" + file.Name
			}
		}(filesChan)
	}

	wg.Wait()

	filesChan <- ""

	wgList.Wait()

	n.stateLock.Lock()

	n.filesList = make([]string, len(filesList))
	copy(n.filesList, filesList)
	n.lastUpdate = time.Now()
	n.lastContact = time.Now()

	n.stateLock.Unlock()
}

func (n *node) GetURL(path string) (string, error) {
	prefix, path, found := strings.Cut(path, ":")
	if !found {
		return "", fmt.Errorf("no prefix provided")
	}

	u := ""

	if prefix == "mem" {
		u = n.address + "/" + filepath.Join("memfs", path)
	} else if prefix == "disk" {
		u = n.address + path
	} else if prefix == "rtmp" {
		u = n.rtmpAddress + path
		if len(n.rtmpToken) != 0 {
			u += "?token=" + url.QueryEscape(n.rtmpToken)
		}
	} else if prefix == "srt" {
		u = n.srtAddress + "?mode=caller"
		if len(n.srtPassphrase) != 0 {
			u += "&passphrase=" + url.QueryEscape(n.srtPassphrase)
		}
		streamid := "#!:m=request,r=" + path
		if len(n.srtToken) != 0 {
			streamid += ",token=" + n.srtToken
		}
		u += "&streamid=" + url.QueryEscape(streamid)
	} else {
		return "", fmt.Errorf("unknown prefix")
	}

	return u, nil
}

func (n *node) GetFile(path string) (io.ReadCloser, error) {
	prefix, path, found := strings.Cut(path, ":")
	if !found {
		return nil, fmt.Errorf("no prefix provided")
	}

	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return nil, fmt.Errorf("not connected")
	}

	if prefix == "mem" {
		return n.peer.MemFSGetFile(path)
	} else if prefix == "disk" {
		return n.peer.DiskFSGetFile(path)
	}

	return nil, fmt.Errorf("unknown prefix")
}

func (n *node) ProcessList() ([]Process, error) {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return nil, fmt.Errorf("not connected")
	}

	list, err := n.peer.ProcessList(client.ProcessListOptions{
		Filter: []string{
			"state",
			"config",
		},
	})
	if err != nil {
		return nil, err
	}

	processes := []Process{}

	for _, p := range list {
		process := Process{
			NodeID:    n.About().ID,
			Order:     p.State.Order,
			State:     p.State.State,
			Mem:       p.State.Memory,
			CPU:       p.State.CPU * n.resources.ncpu,
			Runtime:   time.Duration(p.State.Runtime) * time.Second,
			UpdatedAt: time.Unix(p.UpdatedAt, 0),
		}

		cfg := &app.Config{
			ID:             p.Config.ID,
			Reference:      p.Config.Reference,
			Input:          []app.ConfigIO{},
			Output:         []app.ConfigIO{},
			Options:        p.Config.Options,
			Reconnect:      p.Config.Reconnect,
			ReconnectDelay: p.Config.ReconnectDelay,
			Autostart:      p.Config.Autostart,
			StaleTimeout:   p.Config.StaleTimeout,
			LimitCPU:       p.Config.Limits.CPU,
			LimitMemory:    p.Config.Limits.Memory,
			LimitWaitFor:   p.Config.Limits.WaitFor,
		}

		for _, d := range p.Config.Input {
			cfg.Input = append(cfg.Input, app.ConfigIO{
				ID:      d.ID,
				Address: d.Address,
				Options: d.Options,
			})
		}

		for _, d := range p.Config.Output {
			output := app.ConfigIO{
				ID:      d.ID,
				Address: d.Address,
				Options: d.Options,
				Cleanup: []app.ConfigIOCleanup{},
			}

			for _, c := range d.Cleanup {
				output.Cleanup = append(output.Cleanup, app.ConfigIOCleanup{
					Pattern:       c.Pattern,
					MaxFiles:      c.MaxFiles,
					MaxFileAge:    c.MaxFileAge,
					PurgeOnDelete: c.PurgeOnDelete,
				})
			}

			cfg.Output = append(cfg.Output, output)
		}

		process.Config = cfg

		processes = append(processes, process)
	}

	return processes, nil
}

func (n *node) ProcessAdd(config *app.Config) error {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return fmt.Errorf("not connected")
	}

	cfg := clientapi.ProcessConfig{
		ID:             config.ID,
		Reference:      config.Reference,
		Input:          []clientapi.ProcessConfigIO{},
		Output:         []clientapi.ProcessConfigIO{},
		Options:        config.Options,
		Reconnect:      config.Reconnect,
		ReconnectDelay: config.ReconnectDelay,
		Autostart:      config.Autostart,
		StaleTimeout:   config.StaleTimeout,
		Limits: clientapi.ProcessConfigLimits{
			CPU:     config.LimitCPU,
			Memory:  config.LimitMemory,
			WaitFor: config.LimitWaitFor,
		},
	}

	for _, d := range config.Input {
		cfg.Input = append(cfg.Input, clientapi.ProcessConfigIO{
			ID:      d.ID,
			Address: d.Address,
			Options: d.Options,
		})
	}

	for _, d := range config.Output {
		output := clientapi.ProcessConfigIO{
			ID:      d.ID,
			Address: d.Address,
			Options: d.Options,
			Cleanup: []clientapi.ProcessConfigIOCleanup{},
		}

		for _, c := range d.Cleanup {
			output.Cleanup = append(output.Cleanup, clientapi.ProcessConfigIOCleanup{
				Pattern:       c.Pattern,
				MaxFiles:      c.MaxFiles,
				MaxFileAge:    c.MaxFileAge,
				PurgeOnDelete: c.PurgeOnDelete,
			})
		}

		cfg.Output = append(cfg.Output, output)
	}

	return n.peer.ProcessAdd(cfg)
}

func (n *node) ProcessStart(id string) error {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return fmt.Errorf("not connected")
	}

	return n.peer.ProcessCommand(id, "start")
}

func (n *node) ProcessStop(id string) error {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return fmt.Errorf("not connected")
	}

	return n.peer.ProcessCommand(id, "stop")
}

func (n *node) ProcessDelete(id string) error {
	n.peerLock.RLock()
	defer n.peerLock.RUnlock()

	if n.peer == nil {
		return fmt.Errorf("not connected")
	}

	return n.peer.ProcessDelete(id)
}
