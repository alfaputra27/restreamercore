package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/datarhei/core/v16/iam/access"
	"github.com/datarhei/core/v16/iam/identity"
	"github.com/datarhei/core/v16/log"
	"github.com/datarhei/core/v16/restream/app"

	"github.com/hashicorp/raft"
)

type Store interface {
	raft.FSM

	OnApply(func(op Operation))

	ProcessList() []Process
	GetProcess(id app.ProcessID) (Process, error)

	UserList() Users
	GetUser(name string) Users
	PolicyList() Policies
	PolicyUserList(nam string) Policies
}

type StoreError string

func NewStoreError(format string, a ...any) StoreError {
	return StoreError(fmt.Sprintf(format, a...))
}

func (se StoreError) Error() string {
	return string(se)
}

type Process struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	Config    *app.Config
	Metadata  map[string]interface{}
}

type Users struct {
	UpdatedAt time.Time
	Users     []identity.User
}

type Policies struct {
	UpdatedAt time.Time
	Policies  []access.Policy
}

type Operation string

const (
	OpAddProcess         Operation = "addProcess"
	OpRemoveProcess      Operation = "removeProcess"
	OpUpdateProcess      Operation = "updateProcess"
	OpSetProcessMetadata Operation = "setProcessMetadata"
	OpAddIdentity        Operation = "addIdentity"
	OpUpdateIdentity     Operation = "updateIdentity"
	OpRemoveIdentity     Operation = "removeIdentity"
	OpSetPolicies        Operation = "setPolicies"
)

type Command struct {
	Operation Operation
	Data      interface{}
}

type CommandAddProcess struct {
	Config *app.Config
}

type CommandUpdateProcess struct {
	ID     app.ProcessID
	Config *app.Config
}

type CommandRemoveProcess struct {
	ID app.ProcessID
}

type CommandSetProcessMetadata struct {
	ID   app.ProcessID
	Key  string
	Data interface{}
}

type CommandAddIdentity struct {
	Identity identity.User
}

type CommandUpdateIdentity struct {
	Name     string
	Identity identity.User
}

type CommandRemoveIdentity struct {
	Name string
}

type CommandSetPolicies struct {
	Name     string
	Policies []access.Policy
}

// Implement a FSM
type store struct {
	lock    sync.RWMutex
	Process map[string]Process

	Users struct {
		UpdatedAt time.Time
		Users     map[string]identity.User
	}

	Policies struct {
		UpdatedAt time.Time
		Policies  map[string][]access.Policy
	}

	callback func(op Operation)

	logger log.Logger
}

type Config struct {
	Logger log.Logger
}

func NewStore(config Config) (Store, error) {
	s := &store{
		Process: map[string]Process{},
		logger:  config.Logger,
	}

	s.Users.Users = map[string]identity.User{}
	s.Policies.Policies = map[string][]access.Policy{}

	if s.logger == nil {
		s.logger = log.New("")
	}

	return s, nil
}

func (s *store) Apply(entry *raft.Log) interface{} {
	logger := s.logger.WithFields(log.Fields{
		"index": entry.Index,
		"term":  entry.Term,
	})

	logger.Debug().WithField("data", string(entry.Data)).Log("New entry")

	c := Command{}

	err := json.Unmarshal(entry.Data, &c)
	if err != nil {
		logger.Error().WithError(err).Log("Invalid entry")
		return NewStoreError("invalid log entry, index: %d, term: %d", entry.Index, entry.Term)
	}

	logger.Debug().WithField("operation", c.Operation).Log("")

	switch c.Operation {
	case OpAddProcess:
		b, _ := json.Marshal(c.Data)
		cmd := CommandAddProcess{}
		json.Unmarshal(b, &cmd)

		err = s.addProcess(cmd)
	case OpRemoveProcess:
		b, _ := json.Marshal(c.Data)
		cmd := CommandRemoveProcess{}
		json.Unmarshal(b, &cmd)

		err = s.removeProcess(cmd)
	case OpUpdateProcess:
		b, _ := json.Marshal(c.Data)
		cmd := CommandUpdateProcess{}
		json.Unmarshal(b, &cmd)

		err = s.updateProcess(cmd)
	case OpSetProcessMetadata:
		b, _ := json.Marshal(c.Data)
		cmd := CommandSetProcessMetadata{}
		json.Unmarshal(b, &cmd)

		err = s.setProcessMetadata(cmd)
	case OpAddIdentity:
		b, _ := json.Marshal(c.Data)
		cmd := CommandAddIdentity{}
		json.Unmarshal(b, &cmd)

		err = s.addIdentity(cmd)
	case OpUpdateIdentity:
		b, _ := json.Marshal(c.Data)
		cmd := CommandUpdateIdentity{}
		json.Unmarshal(b, &cmd)

		err = s.updateIdentity(cmd)
	case OpRemoveIdentity:
		b, _ := json.Marshal(c.Data)
		cmd := CommandRemoveIdentity{}
		json.Unmarshal(b, &cmd)

		err = s.removeIdentity(cmd)
	case OpSetPolicies:
		b, _ := json.Marshal(c.Data)
		cmd := CommandSetPolicies{}
		json.Unmarshal(b, &cmd)

		err = s.setPolicies(cmd)
	default:
		s.logger.Warn().WithField("operation", c.Operation).Log("Unknown operation")
		return nil
	}

	if err != nil {
		logger.Debug().WithError(err).WithField("operation", c.Operation).Log("")
		return err
	}

	s.lock.RLock()
	if s.callback != nil {
		s.callback(c.Operation)
	}
	s.lock.RUnlock()

	return nil
}

func (s *store) addProcess(cmd CommandAddProcess) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	id := cmd.Config.ProcessID().String()

	_, ok := s.Process[id]
	if ok {
		return NewStoreError("the process with the ID '%s' already exists", id)
	}

	now := time.Now()
	s.Process[id] = Process{
		CreatedAt: now,
		UpdatedAt: now,
		Config:    cmd.Config,
		Metadata:  map[string]interface{}{},
	}

	return nil
}

func (s *store) removeProcess(cmd CommandRemoveProcess) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	id := cmd.ID.String()

	_, ok := s.Process[id]
	if !ok {
		return NewStoreError("the process with the ID '%s' doesn't exist", id)
	}

	delete(s.Process, id)

	return nil
}

func (s *store) updateProcess(cmd CommandUpdateProcess) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	srcid := cmd.ID.String()
	dstid := cmd.Config.ProcessID().String()

	p, ok := s.Process[srcid]
	if !ok {
		return NewStoreError("the process with the ID '%s' doesn't exists", srcid)
	}

	currentHash := p.Config.Hash()
	replaceHash := cmd.Config.Hash()

	if bytes.Equal(currentHash, replaceHash) {
		return nil
	}

	if srcid == dstid {
		s.Process[srcid] = Process{
			UpdatedAt: time.Now(),
			Config:    cmd.Config,
		}
	} else {
		_, ok := s.Process[dstid]
		if ok {
			return NewStoreError("the process with the ID '%s' already exists", dstid)
		}

		delete(s.Process, srcid)
		s.Process[dstid] = Process{
			UpdatedAt: time.Now(),
			Config:    cmd.Config,
		}
	}

	return nil
}

func (s *store) setProcessMetadata(cmd CommandSetProcessMetadata) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	id := cmd.ID.String()

	p, ok := s.Process[id]
	if !ok {
		return NewStoreError("the process with the ID '%s' doesn't exists", cmd.ID)
	}

	if p.Metadata == nil {
		p.Metadata = map[string]interface{}{}
	}

	if cmd.Data == nil {
		delete(p.Metadata, cmd.Key)
	} else {
		p.Metadata[cmd.Key] = cmd.Data
	}
	p.UpdatedAt = time.Now()

	s.Process[id] = p

	return nil
}

func (s *store) addIdentity(cmd CommandAddIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	_, ok := s.Users.Users[cmd.Identity.Name]
	if ok {
		return NewStoreError("the identity with the name '%s' already exists", cmd.Identity.Name)
	}

	s.Users.UpdatedAt = time.Now()
	s.Users.Users[cmd.Identity.Name] = cmd.Identity

	return nil
}

func (s *store) updateIdentity(cmd CommandUpdateIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	_, ok := s.Users.Users[cmd.Name]
	if ok {
		if cmd.Name == cmd.Identity.Name {
			s.Users.UpdatedAt = time.Now()
			s.Users.Users[cmd.Identity.Name] = cmd.Identity
		} else {
			_, ok := s.Users.Users[cmd.Identity.Name]
			if !ok {
				s.Users.UpdatedAt = time.Now()
				s.Users.Users[cmd.Identity.Name] = cmd.Identity
			} else {
				return NewStoreError("the identity with the name '%s' already exists", cmd.Identity.Name)
			}
		}
	}

	return nil
}

func (s *store) removeIdentity(cmd CommandRemoveIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.Users.Users, cmd.Name)
	s.Users.UpdatedAt = time.Now()
	delete(s.Policies.Policies, cmd.Name)
	s.Policies.UpdatedAt = time.Now()

	return nil
}

func (s *store) setPolicies(cmd CommandSetPolicies) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.Policies.Policies, cmd.Name)
	s.Policies.Policies[cmd.Name] = cmd.Policies
	s.Policies.UpdatedAt = time.Now()

	return nil
}

func (s *store) OnApply(fn func(op Operation)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.callback = fn
}

func (s *store) Snapshot() (raft.FSMSnapshot, error) {
	s.logger.Debug().Log("Snapshot request")

	s.lock.RLock()
	defer s.lock.RUnlock()

	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	return &fsmSnapshot{
		data: data,
	}, nil
}

func (s *store) Restore(snapshot io.ReadCloser) error {
	s.logger.Debug().Log("Snapshot restore")

	defer snapshot.Close()

	s.lock.Lock()
	defer s.lock.Unlock()

	dec := json.NewDecoder(snapshot)
	if err := dec.Decode(s); err != nil {
		return err
	}

	for id, p := range s.Process {
		if p.Metadata != nil {
			continue
		}

		p.Metadata = map[string]interface{}{}
		s.Process[id] = p
	}

	return nil
}

func (s *store) ProcessList() []Process {
	s.lock.RLock()
	defer s.lock.RUnlock()

	processes := []Process{}

	for _, p := range s.Process {
		processes = append(processes, Process{
			CreatedAt: p.CreatedAt,
			UpdatedAt: p.UpdatedAt,
			Config:    p.Config.Clone(),
			Metadata:  p.Metadata,
		})
	}

	return processes
}

func (s *store) GetProcess(id app.ProcessID) (Process, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	process, ok := s.Process[id.String()]
	if !ok {
		return Process{}, fmt.Errorf("not found")
	}

	return Process{
		CreatedAt: process.CreatedAt,
		UpdatedAt: process.UpdatedAt,
		Config:    process.Config.Clone(),
		Metadata:  process.Metadata,
	}, nil
}

func (s *store) UserList() Users {
	s.lock.RLock()
	defer s.lock.RUnlock()

	u := Users{
		UpdatedAt: s.Users.UpdatedAt,
	}

	for _, user := range s.Users.Users {
		u.Users = append(u.Users, user)
	}

	return u
}

func (s *store) GetUser(name string) Users {
	s.lock.RLock()
	defer s.lock.RUnlock()

	u := Users{
		UpdatedAt: s.Users.UpdatedAt,
	}

	if user, ok := s.Users.Users[name]; ok {
		u.Users = append(u.Users, user)
	}

	return u
}

func (s *store) PolicyList() Policies {
	s.lock.RLock()
	defer s.lock.RUnlock()

	p := Policies{
		UpdatedAt: s.Policies.UpdatedAt,
	}

	for _, policies := range s.Policies.Policies {
		p.Policies = append(p.Policies, policies...)
	}

	return p
}

func (s *store) PolicyUserList(name string) Policies {
	s.lock.RLock()
	defer s.lock.RUnlock()

	p := Policies{
		UpdatedAt: s.Policies.UpdatedAt,
	}

	p.Policies = append(p.Policies, s.Policies.Policies[name]...)

	return p
}

type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}

	sink.Close()
	return nil
}

func (s *fsmSnapshot) Release() {
	s.data = nil
}
