package forwarder

import (
	"io"
	"net/http"
	"sync"
	"time"

	apiclient "github.com/datarhei/core/v16/cluster/client"
	iamaccess "github.com/datarhei/core/v16/iam/access"
	iamidentity "github.com/datarhei/core/v16/iam/identity"
	"github.com/datarhei/core/v16/log"
	"github.com/datarhei/core/v16/restream/app"
)

// Forwarder forwards any HTTP request from a follower to the leader
type Forwarder interface {
	SetLeader(address string)
	HasLeader() bool

	Join(origin, id, raftAddress, peerAddress string) error
	Leave(origin, id string) error
	Snapshot() (io.ReadCloser, error)

	AddProcess(origin string, config *app.Config) error
	UpdateProcess(origin, id string, config *app.Config) error
	RemoveProcess(origin, id string) error

	AddIdentity(origin string, identity iamidentity.User) error
	UpdateIdentity(origin, name string, identity iamidentity.User) error
	SetPolicies(origin, name string, policies []iamaccess.Policy) error
	RemoveIdentity(origin string, name string) error
}

type forwarder struct {
	id   string
	lock sync.RWMutex

	client apiclient.APIClient

	logger log.Logger
}

type ForwarderConfig struct {
	ID     string
	Logger log.Logger
}

func New(config ForwarderConfig) (Forwarder, error) {
	f := &forwarder{
		id:     config.ID,
		logger: config.Logger,
	}

	if f.logger == nil {
		f.logger = log.New("")
	}

	tr := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}

	f.client = apiclient.APIClient{
		Client: client,
	}

	return f, nil
}

func (f *forwarder) SetLeader(address string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.client.Address == address {
		return
	}

	f.logger.Debug().Log("Setting leader address to %s", address)

	f.client.Address = address
}

func (f *forwarder) HasLeader() bool {
	return len(f.client.Address) != 0
}

func (f *forwarder) Join(origin, id, raftAddress, peerAddress string) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.JoinRequest{
		ID:          id,
		RaftAddress: raftAddress,
	}

	f.logger.Debug().WithField("request", r).Log("Forwarding to leader")

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	if len(peerAddress) != 0 {
		client = apiclient.APIClient{
			Address: peerAddress,
			Client:  f.client.Client,
		}
	}

	return client.Join(origin, r)
}

func (f *forwarder) Leave(origin, id string) error {
	if origin == "" {
		origin = f.id
	}

	f.logger.Debug().WithField("id", id).Log("Forwarding to leader")

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.Leave(origin, id)
}

func (f *forwarder) Snapshot() (io.ReadCloser, error) {
	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.Snapshot()
}

func (f *forwarder) AddProcess(origin string, config *app.Config) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.AddProcessRequest{
		Config: *config,
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.AddProcess(origin, r)
}

func (f *forwarder) UpdateProcess(origin, id string, config *app.Config) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.UpdateProcessRequest{
		ID:     id,
		Config: *config,
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.UpdateProcess(origin, r)
}

func (f *forwarder) RemoveProcess(origin, id string) error {
	if origin == "" {
		origin = f.id
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.RemoveProcess(origin, id)
}

func (f *forwarder) AddIdentity(origin string, identity iamidentity.User) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.AddIdentityRequest{
		Identity: identity,
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.AddIdentity(origin, r)
}

func (f *forwarder) UpdateIdentity(origin, name string, identity iamidentity.User) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.UpdateIdentityRequest{
		Name:     name,
		Identity: identity,
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.UpdateIdentity(origin, name, r)
}

func (f *forwarder) SetPolicies(origin, name string, policies []iamaccess.Policy) error {
	if origin == "" {
		origin = f.id
	}

	r := apiclient.SetPoliciesRequest{
		Name:     name,
		Policies: policies,
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.SetPolicies(origin, name, r)
}

func (f *forwarder) RemoveIdentity(origin string, name string) error {
	if origin == "" {
		origin = f.id
	}

	f.lock.RLock()
	client := f.client
	f.lock.RUnlock()

	return client.RemoveIdentity(origin, name)
}