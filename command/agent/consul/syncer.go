// Package consul is used by Nomad to register all services both static services
// and dynamic via allocations.
//
// Consul Service IDs have the following format: ${nomadServicePrefix}-${groupName}-${serviceKey}
// groupName takes on one of the following values:
// - server
// - client
// - executor-${alloc-id}-${task-name}
//
// serviceKey should be generated by service registrators.
// If the serviceKey is being generated by the executor for a Nomad Task.Services
// the following helper should be used:
//    NOTE: Executor should interpolate the service prior to calling
//    func GenerateTaskServiceKey(service *structs.Service) string
//
// The Nomad Client reaps services registered from dead allocations that were
// not properly cleaned up by the executor (this is not the expected case).
//
// TODO fix this comment
// The Consul ServiceIDs generated by the executor will contain the allocation
// ID. Thus the client can generate the list of Consul ServiceIDs to keep by
// calling the following method on all running allocations the client is aware
// of:
// func GenerateExecutorServiceKeyPrefixFromAlloc(allocID string) string
package consul

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/go-multierror"

	cconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/nomad/types"
)

const (
	// initialSyncBuffer is the max time an initial sync will sleep
	// before syncing.
	initialSyncBuffer = 30 * time.Second

	// initialSyncDelay is the delay before an initial sync.
	initialSyncDelay = 5 * time.Second

	// nomadServicePrefix is the first prefix that scopes all Nomad registered
	// services
	nomadServicePrefix = "_nomad"

	// The periodic time interval for syncing services and checks with Consul
	syncInterval = 5 * time.Second

	// syncJitter provides a little variance in the frequency at which
	// Syncer polls Consul.
	syncJitter = 8

	// ttlCheckBuffer is the time interval that Nomad can take to report Consul
	// the check result
	ttlCheckBuffer = 31 * time.Second

	// DefaultQueryWaitDuration is the max duration the Consul Agent will
	// spend waiting for a response from a Consul Query.
	DefaultQueryWaitDuration = 2 * time.Second

	// ServiceTagHTTP is the tag assigned to HTTP services
	ServiceTagHTTP = "http"

	// ServiceTagRPC is the tag assigned to RPC services
	ServiceTagRPC = "rpc"

	// ServiceTagSerf is the tag assigned to Serf services
	ServiceTagSerf = "serf"
)

// consulServiceID and consulCheckID are the IDs registered with Consul
type consulServiceID string
type consulCheckID string

// ServiceKey is the generated service key that is used to build the Consul
// ServiceID
type ServiceKey string

// ServiceDomain is the domain of services registered by Nomad
type ServiceDomain string

const (
	ClientDomain ServiceDomain = "client"
	ServerDomain ServiceDomain = "server"
)

// NewExecutorDomain returns a domain specific to the alloc ID and task
func NewExecutorDomain(allocID, task string) ServiceDomain {
	return ServiceDomain(fmt.Sprintf("executor-%s-%s", allocID, task))
}

// Syncer allows syncing of services and checks with Consul
type Syncer struct {
	client          *consul.Client
	consulAvailable bool

	// servicesGroups and checkGroups are named groups of services and checks
	// respectively that will be flattened and reconciled with Consul when
	// SyncServices() is called. The key to the servicesGroups map is unique
	// per handler and is used to allow the Agent's services to be maintained
	// independently of the Client or Server's services.
	servicesGroups map[ServiceDomain]map[ServiceKey]*consul.AgentServiceRegistration
	checkGroups    map[ServiceDomain]map[ServiceKey][]*consul.AgentCheckRegistration
	groupsLock     sync.RWMutex

	// The "Consul Registry" is a collection of Consul Services and
	// Checks all guarded by the registryLock.
	registryLock sync.RWMutex

	// trackedChecks and trackedServices are registered with consul
	trackedChecks   map[consulCheckID]*consul.AgentCheckRegistration
	trackedServices map[consulServiceID]*consul.AgentServiceRegistration

	// checkRunners are delegated Consul checks being ran by the Syncer
	checkRunners map[consulCheckID]*CheckRunner

	addrFinder           func(portLabel string) (string, int)
	createDelegatedCheck func(*structs.ServiceCheck, string) (Check, error)
	delegateChecks       map[string]struct{} // delegateChecks are the checks that the Nomad client runs and reports to Consul
	// End registryLock guarded attributes.

	logger *log.Logger

	shutdownCh   chan struct{}
	shutdown     bool
	shutdownLock sync.Mutex

	// notifyShutdownCh is used to notify a Syncer it needs to shutdown.
	// This can happen because there was an explicit call to the Syncer's
	// Shutdown() method, or because the calling task signaled the
	// program is going to exit by closing its shutdownCh.
	notifyShutdownCh chan struct{}

	// periodicCallbacks is walked sequentially when the timer in Run
	// fires.
	periodicCallbacks map[string]types.PeriodicCallback
	notifySyncCh      chan struct{}
	periodicLock      sync.RWMutex
}

// NewSyncer returns a new consul.Syncer
func NewSyncer(consulConfig *config.ConsulConfig, shutdownCh chan struct{}, logger *log.Logger) (*Syncer, error) {
	var err error
	var c *consul.Client

	cfg := consul.DefaultConfig()

	// If a nil consulConfig was provided, fall back to the default config
	if consulConfig == nil {
		consulConfig = cconfig.DefaultConfig().ConsulConfig
	}

	if consulConfig.Addr != "" {
		cfg.Address = consulConfig.Addr
	}
	if consulConfig.Token != "" {
		cfg.Token = consulConfig.Token
	}
	if consulConfig.Auth != "" {
		var username, password string
		if strings.Contains(consulConfig.Auth, ":") {
			split := strings.SplitN(consulConfig.Auth, ":", 2)
			username = split[0]
			password = split[1]
		} else {
			username = consulConfig.Auth
		}

		cfg.HttpAuth = &consul.HttpBasicAuth{
			Username: username,
			Password: password,
		}
	}
	if consulConfig.EnableSSL {
		cfg.Scheme = "https"
		tlsCfg := consul.TLSConfig{
			Address:            cfg.Address,
			CAFile:             consulConfig.CAFile,
			CertFile:           consulConfig.CertFile,
			KeyFile:            consulConfig.KeyFile,
			InsecureSkipVerify: !consulConfig.VerifySSL,
		}
		tlsClientCfg, err := consul.SetupTLSConfig(&tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("error creating tls client config for consul: %v", err)
		}
		cfg.HttpClient.Transport = &http.Transport{
			TLSClientConfig: tlsClientCfg,
		}
	}
	if consulConfig.EnableSSL && !consulConfig.VerifySSL {
		cfg.HttpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}
	if c, err = consul.NewClient(cfg); err != nil {
		return nil, err
	}
	consulSyncer := Syncer{
		client:            c,
		logger:            logger,
		consulAvailable:   true,
		shutdownCh:        shutdownCh,
		servicesGroups:    make(map[ServiceDomain]map[ServiceKey]*consul.AgentServiceRegistration),
		checkGroups:       make(map[ServiceDomain]map[ServiceKey][]*consul.AgentCheckRegistration),
		trackedServices:   make(map[consulServiceID]*consul.AgentServiceRegistration),
		trackedChecks:     make(map[consulCheckID]*consul.AgentCheckRegistration),
		checkRunners:      make(map[consulCheckID]*CheckRunner),
		periodicCallbacks: make(map[string]types.PeriodicCallback),
	}

	return &consulSyncer, nil
}

// SetDelegatedChecks sets the checks that nomad is going to run and report the
// result back to consul
func (c *Syncer) SetDelegatedChecks(delegateChecks map[string]struct{}, createDelegatedCheckFn func(*structs.ServiceCheck, string) (Check, error)) *Syncer {
	c.delegateChecks = delegateChecks
	c.createDelegatedCheck = createDelegatedCheckFn
	return c
}

// SetAddrFinder sets a function to find the host and port for a Service given its port label
func (c *Syncer) SetAddrFinder(addrFinder func(string) (string, int)) *Syncer {
	c.addrFinder = addrFinder
	return c
}

// GenerateServiceKey should be called to generate a serviceKey based on the
// Service.
func GenerateServiceKey(service *structs.Service) ServiceKey {
	var key string
	numTags := len(service.Tags)
	switch numTags {
	case 0:
		key = fmt.Sprintf("%s", service.Name)
	default:
		tags := strings.Join(service.Tags, "-")
		key = fmt.Sprintf("%s-%s", service.Name, tags)
	}
	return ServiceKey(key)
}

// SetServices stores the map of Nomad Services to the provided service
// domain name.
func (c *Syncer) SetServices(domain ServiceDomain, services map[ServiceKey]*structs.Service) error {
	var mErr multierror.Error
	numServ := len(services)
	registeredServices := make(map[ServiceKey]*consul.AgentServiceRegistration, numServ)
	registeredChecks := make(map[ServiceKey][]*consul.AgentCheckRegistration, numServ)
	for serviceKey, service := range services {
		serviceReg, err := c.createService(service, domain, serviceKey)
		if err != nil {
			mErr.Errors = append(mErr.Errors, err)
			continue
		}
		registeredServices[serviceKey] = serviceReg

		// Register the check(s) for this service
		for _, chk := range service.Checks {
			// Create a Consul check registration
			chkReg, err := c.createCheckReg(chk, serviceReg)
			if err != nil {
				mErr.Errors = append(mErr.Errors, err)
				continue
			}

			// creating a nomad check if we have to handle this particular check type
			c.registryLock.RLock()
			if _, ok := c.delegateChecks[chk.Type]; ok {
				_, ok := c.checkRunners[consulCheckID(chkReg.ID)]
				c.registryLock.RUnlock()
				if ok {
					continue
				}

				nc, err := c.createDelegatedCheck(chk, chkReg.ID)
				if err != nil {
					mErr.Errors = append(mErr.Errors, err)
					continue
				}

				cr := NewCheckRunner(nc, c.runCheck, c.logger)
				c.registryLock.Lock()
				// TODO type the CheckRunner
				c.checkRunners[consulCheckID(nc.ID())] = cr
				c.registryLock.Unlock()
			} else {
				c.registryLock.RUnlock()
			}

			registeredChecks[serviceKey] = append(registeredChecks[serviceKey], chkReg)
		}
	}

	if len(mErr.Errors) > 0 {
		return mErr.ErrorOrNil()
	}

	c.groupsLock.Lock()
	for serviceKey, service := range registeredServices {
		serviceKeys, ok := c.servicesGroups[domain]
		if !ok {
			serviceKeys = make(map[ServiceKey]*consul.AgentServiceRegistration, len(registeredServices))
			c.servicesGroups[domain] = serviceKeys
		}
		serviceKeys[serviceKey] = service
	}
	for serviceKey, checks := range registeredChecks {
		serviceKeys, ok := c.checkGroups[domain]
		if !ok {
			serviceKeys = make(map[ServiceKey][]*consul.AgentCheckRegistration, len(registeredChecks))
			c.checkGroups[domain] = serviceKeys
		}
		serviceKeys[serviceKey] = checks
	}
	c.groupsLock.Unlock()

	// Sync immediately
	c.SyncNow()

	return nil
}

// SyncNow expires the current timer forcing the list of periodic callbacks
// to be synced immediately.
func (c *Syncer) SyncNow() {
	select {
	case c.notifySyncCh <- struct{}{}:
	default:
	}
}

// flattenedServices returns a flattened list of services that are registered
// locally
func (c *Syncer) flattenedServices() []*consul.AgentServiceRegistration {
	const initialNumServices = 8
	services := make([]*consul.AgentServiceRegistration, 0, initialNumServices)
	c.groupsLock.RLock()
	defer c.groupsLock.RUnlock()
	for _, servicesGroup := range c.servicesGroups {
		for _, service := range servicesGroup {
			services = append(services, service)
		}
	}
	return services
}

// flattenedChecks returns a flattened list of checks that are registered
// locally
func (c *Syncer) flattenedChecks() []*consul.AgentCheckRegistration {
	const initialNumChecks = 8
	checks := make([]*consul.AgentCheckRegistration, 0, initialNumChecks)
	c.groupsLock.RLock()
	for _, checkGroup := range c.checkGroups {
		for _, check := range checkGroup {
			checks = append(checks, check...)
		}
	}
	c.groupsLock.RUnlock()
	return checks
}

func (c *Syncer) signalShutdown() {
	select {
	case c.notifyShutdownCh <- struct{}{}:
	default:
	}
}

// Shutdown de-registers the services and checks and shuts down periodic syncing
func (c *Syncer) Shutdown() error {
	var mErr multierror.Error

	c.shutdownLock.Lock()
	if !c.shutdown {
		c.shutdown = true
	}
	c.shutdownLock.Unlock()

	c.signalShutdown()

	// Stop all the checks that nomad is running
	c.registryLock.RLock()
	defer c.registryLock.RUnlock()
	for _, cr := range c.checkRunners {
		cr.Stop()
	}

	// De-register all the services from Consul
	for serviceID := range c.trackedServices {
		convertedID := string(serviceID)
		if err := c.client.Agent().ServiceDeregister(convertedID); err != nil {
			c.logger.Printf("[WARN] consul.syncer: failed to deregister service ID %+q: %v", convertedID, err)
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	return mErr.ErrorOrNil()
}

// queryChecks queries the Consul Agent for a list of Consul checks that
// have been registered with this Consul Syncer.
func (c *Syncer) queryChecks() (map[consulCheckID]*consul.AgentCheck, error) {
	checks, err := c.client.Agent().Checks()
	if err != nil {
		return nil, err
	}
	return c.filterConsulChecks(checks), nil
}

// queryAgentServices queries the Consul Agent for a list of Consul services that
// have been registered with this Consul Syncer.
func (c *Syncer) queryAgentServices() (map[consulServiceID]*consul.AgentService, error) {
	services, err := c.client.Agent().Services()
	if err != nil {
		return nil, err
	}
	return c.filterConsulServices(services), nil
}

// syncChecks synchronizes this Syncer's Consul Checks with the Consul Agent.
func (c *Syncer) syncChecks() error {
	var mErr multierror.Error
	consulChecks, err := c.queryChecks()
	if err != nil {
		return err
	}

	// Synchronize checks with Consul
	missingChecks, _, changedChecks, staleChecks := c.calcChecksDiff(consulChecks)
	for _, check := range missingChecks {
		if err := c.registerCheck(check); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		c.registryLock.Lock()
		c.trackedChecks[consulCheckID(check.ID)] = check
		c.registryLock.Unlock()
	}
	for _, check := range changedChecks {
		// NOTE(sean@): Do we need to deregister the check before
		// re-registering it?  Not deregistering to avoid missing the
		// TTL but doesn't correct reconcile any possible drift with
		// the check.
		//
		// if err := c.deregisterCheck(check.ID); err != nil {
		//   mErr.Errors = append(mErr.Errors, err)
		// }
		if err := c.registerCheck(check); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	for _, check := range staleChecks {
		if err := c.deregisterCheck(consulCheckID(check.ID)); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		c.registryLock.Lock()
		delete(c.trackedChecks, consulCheckID(check.ID))
		c.registryLock.Unlock()
	}
	return mErr.ErrorOrNil()
}

// compareConsulCheck takes a consul.AgentCheckRegistration instance and
// compares it with a consul.AgentCheck.  Returns true if they are equal
// according to consul.AgentCheck, otherwise false.
func compareConsulCheck(localCheck *consul.AgentCheckRegistration, consulCheck *consul.AgentCheck) bool {
	if consulCheck.CheckID != localCheck.ID ||
		consulCheck.Name != localCheck.Name ||
		consulCheck.Notes != localCheck.Notes ||
		consulCheck.ServiceID != localCheck.ServiceID {
		return false
	}
	return true
}

// calcChecksDiff takes the argument (consulChecks) and calculates the delta
// between the consul.Syncer's list of known checks (c.trackedChecks).  Three
// arrays are returned:
//
// 1) a slice of checks that exist only locally in the Syncer and are missing
// from the Consul Agent (consulChecks) and therefore need to be registered.
//
// 2) a slice of checks that exist in both the local consul.Syncer's
// tracked list and Consul Agent (consulChecks).
//
// 3) a slice of checks that exist in both the local consul.Syncer's
// tracked list and Consul Agent (consulServices) but have diverged state.
//
// 4) a slice of checks that exist only in the Consul Agent (consulChecks)
// and should be removed because the Consul Agent has drifted from the
// Syncer.
func (c *Syncer) calcChecksDiff(consulChecks map[consulCheckID]*consul.AgentCheck) (
	missingChecks []*consul.AgentCheckRegistration,
	equalChecks []*consul.AgentCheckRegistration,
	changedChecks []*consul.AgentCheckRegistration,
	staleChecks []*consul.AgentCheckRegistration) {

	type mergedCheck struct {
		check *consul.AgentCheckRegistration
		// 'l' == Nomad local only
		// 'e' == equal
		// 'c' == changed
		// 'a' == Consul agent only
		state byte
	}
	var (
		localChecksCount   = 0
		equalChecksCount   = 0
		changedChecksCount = 0
		agentChecks        = 0
	)
	c.registryLock.RLock()
	localChecks := make(map[string]*mergedCheck, len(c.trackedChecks)+len(consulChecks))
	for _, localCheck := range c.flattenedChecks() {
		localChecksCount++
		localChecks[localCheck.ID] = &mergedCheck{localCheck, 'l'}
	}
	c.registryLock.RUnlock()
	for _, consulCheck := range consulChecks {
		if localCheck, found := localChecks[consulCheck.CheckID]; found {
			localChecksCount--
			if compareConsulCheck(localCheck.check, consulCheck) {
				equalChecksCount++
				localChecks[consulCheck.CheckID].state = 'e'
			} else {
				changedChecksCount++
				localChecks[consulCheck.CheckID].state = 'c'
			}
		} else {
			agentChecks++
			agentCheckReg := &consul.AgentCheckRegistration{
				ID:        consulCheck.CheckID,
				Name:      consulCheck.Name,
				Notes:     consulCheck.Notes,
				ServiceID: consulCheck.ServiceID,
			}
			localChecks[consulCheck.CheckID] = &mergedCheck{agentCheckReg, 'a'}
		}
	}

	missingChecks = make([]*consul.AgentCheckRegistration, 0, localChecksCount)
	equalChecks = make([]*consul.AgentCheckRegistration, 0, equalChecksCount)
	changedChecks = make([]*consul.AgentCheckRegistration, 0, changedChecksCount)
	staleChecks = make([]*consul.AgentCheckRegistration, 0, agentChecks)
	for _, check := range localChecks {
		switch check.state {
		case 'l':
			missingChecks = append(missingChecks, check.check)
		case 'e':
			equalChecks = append(equalChecks, check.check)
		case 'c':
			changedChecks = append(changedChecks, check.check)
		case 'a':
			staleChecks = append(staleChecks, check.check)
		}
	}

	return missingChecks, equalChecks, changedChecks, staleChecks
}

// compareConsulService takes a consul.AgentServiceRegistration instance and
// compares it with a consul.AgentService.  Returns true if they are equal
// according to consul.AgentService, otherwise false.
func compareConsulService(localService *consul.AgentServiceRegistration, consulService *consul.AgentService) bool {
	if consulService.ID != localService.ID ||
		consulService.Service != localService.Name ||
		consulService.Port != localService.Port ||
		consulService.Address != localService.Address ||
		consulService.EnableTagOverride != localService.EnableTagOverride {
		return false
	}

	serviceTags := make(map[string]byte, len(localService.Tags))
	for _, tag := range localService.Tags {
		serviceTags[tag] = 'l'
	}
	for _, tag := range consulService.Tags {
		if _, found := serviceTags[tag]; !found {
			return false
		}
		serviceTags[tag] = 'b'
	}
	for _, state := range serviceTags {
		if state == 'l' {
			return false
		}
	}

	return true
}

// calcServicesDiff takes the argument (consulServices) and calculates the
// delta between the consul.Syncer's list of known services
// (c.trackedServices).  Four arrays are returned:
//
// 1) a slice of services that exist only locally in the Syncer and are
// missing from the Consul Agent (consulServices) and therefore need to be
// registered.
//
// 2) a slice of services that exist in both the local consul.Syncer's
// tracked list and Consul Agent (consulServices) *AND* are identical.
//
// 3) a slice of services that exist in both the local consul.Syncer's
// tracked list and Consul Agent (consulServices) but have diverged state.
//
// 4) a slice of services that exist only in the Consul Agent
// (consulServices) and should be removed because the Consul Agent has
// drifted from the Syncer.
func (c *Syncer) calcServicesDiff(consulServices map[consulServiceID]*consul.AgentService) (missingServices []*consul.AgentServiceRegistration, equalServices []*consul.AgentServiceRegistration, changedServices []*consul.AgentServiceRegistration, staleServices []*consul.AgentServiceRegistration) {
	type mergedService struct {
		service *consul.AgentServiceRegistration
		// 'l' == Nomad local only
		// 'e' == equal
		// 'c' == changed
		// 'a' == Consul agent only
		state byte
	}
	var (
		localServicesCount   = 0
		equalServicesCount   = 0
		changedServicesCount = 0
		agentServices        = 0
	)
	c.registryLock.RLock()
	localServices := make(map[string]*mergedService, len(c.trackedServices)+len(consulServices))
	c.registryLock.RUnlock()
	for _, localService := range c.flattenedServices() {
		localServicesCount++
		localServices[localService.ID] = &mergedService{localService, 'l'}
	}
	for _, consulService := range consulServices {
		if localService, found := localServices[consulService.ID]; found {
			localServicesCount--
			if compareConsulService(localService.service, consulService) {
				equalServicesCount++
				localServices[consulService.ID].state = 'e'
			} else {
				changedServicesCount++
				localServices[consulService.ID].state = 'c'
			}
		} else {
			agentServices++
			agentServiceReg := &consul.AgentServiceRegistration{
				ID:      consulService.ID,
				Name:    consulService.Service,
				Tags:    consulService.Tags,
				Port:    consulService.Port,
				Address: consulService.Address,
			}
			localServices[consulService.ID] = &mergedService{agentServiceReg, 'a'}
		}
	}

	missingServices = make([]*consul.AgentServiceRegistration, 0, localServicesCount)
	equalServices = make([]*consul.AgentServiceRegistration, 0, equalServicesCount)
	changedServices = make([]*consul.AgentServiceRegistration, 0, changedServicesCount)
	staleServices = make([]*consul.AgentServiceRegistration, 0, agentServices)
	for _, service := range localServices {
		switch service.state {
		case 'l':
			missingServices = append(missingServices, service.service)
		case 'e':
			equalServices = append(equalServices, service.service)
		case 'c':
			changedServices = append(changedServices, service.service)
		case 'a':
			staleServices = append(staleServices, service.service)
		}
	}

	return missingServices, equalServices, changedServices, staleServices
}

// syncServices synchronizes this Syncer's Consul Services with the Consul
// Agent.
func (c *Syncer) syncServices() error {
	consulServices, err := c.queryAgentServices()
	if err != nil {
		return err
	}

	// Synchronize services with Consul
	var mErr multierror.Error
	missingServices, _, changedServices, removedServices := c.calcServicesDiff(consulServices)
	for _, service := range missingServices {
		if err := c.client.Agent().ServiceRegister(service); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		c.registryLock.Lock()
		c.trackedServices[consulServiceID(service.ID)] = service
		c.registryLock.Unlock()
	}
	for _, service := range changedServices {
		// Re-register the local service
		if err := c.client.Agent().ServiceRegister(service); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	for _, service := range removedServices {
		if err := c.deregisterService(service.ID); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		c.registryLock.Lock()
		delete(c.trackedServices, consulServiceID(service.ID))
		c.registryLock.Unlock()
	}
	return mErr.ErrorOrNil()
}

// registerCheck registers a check definition with Consul
func (c *Syncer) registerCheck(chkReg *consul.AgentCheckRegistration) error {
	c.registryLock.RLock()
	if cr, ok := c.checkRunners[consulCheckID(chkReg.ID)]; ok {
		cr.Start()
	}
	c.registryLock.RUnlock()
	return c.client.Agent().CheckRegister(chkReg)
}

// createCheckReg creates a Check that can be registered with Nomad. It also
// creates a Nomad check for the check types that it can handle.
func (c *Syncer) createCheckReg(check *structs.ServiceCheck, service *consul.AgentServiceRegistration) (*consul.AgentCheckRegistration, error) {
	chkReg := consul.AgentCheckRegistration{
		ID:        check.Hash(service.ID),
		Name:      check.Name,
		ServiceID: service.ID,
	}
	chkReg.Timeout = check.Timeout.String()
	chkReg.Interval = check.Interval.String()
	switch check.Type {
	case structs.ServiceCheckHTTP:
		if check.Protocol == "" {
			check.Protocol = "http"
		}
		url := url.URL{
			Scheme: check.Protocol,
			Host:   fmt.Sprintf("%s:%d", service.Address, service.Port),
			Path:   check.Path,
		}
		chkReg.HTTP = url.String()
	case structs.ServiceCheckTCP:
		chkReg.TCP = fmt.Sprintf("%s:%d", service.Address, service.Port)
	case structs.ServiceCheckScript:
		chkReg.TTL = (check.Interval + ttlCheckBuffer).String()
	default:
		return nil, fmt.Errorf("check type %+q not valid", check.Type)
	}
	return &chkReg, nil
}

// generateConsulServiceID takes the domain and service key and returns a Consul
// ServiceID
func generateConsulServiceID(domain ServiceDomain, key ServiceKey) consulServiceID {
	return consulServiceID(fmt.Sprintf("%s-%s-%s", nomadServicePrefix, domain, key))
}

// createService creates a Consul AgentService from a Nomad ConsulService.
func (c *Syncer) createService(service *structs.Service, domain ServiceDomain, key ServiceKey) (*consul.AgentServiceRegistration, error) {
	c.registryLock.RLock()
	defer c.registryLock.RUnlock()

	srv := consul.AgentServiceRegistration{
		ID:   string(generateConsulServiceID(domain, key)),
		Name: service.Name,
		Tags: service.Tags,
	}
	host, port := c.addrFinder(service.PortLabel)
	if host != "" {
		srv.Address = host
	}

	if port != 0 {
		srv.Port = port
	}

	return &srv, nil
}

// deregisterService de-registers a service with the given ID from consul
func (c *Syncer) deregisterService(serviceID string) error {
	return c.client.Agent().ServiceDeregister(serviceID)
}

// deregisterCheck de-registers a check from Consul
func (c *Syncer) deregisterCheck(id consulCheckID) error {
	c.registryLock.Lock()
	defer c.registryLock.Unlock()

	// Deleting from Consul Agent
	if err := c.client.Agent().CheckDeregister(string(id)); err != nil {
		// CheckDeregister() will be reattempted again in a future
		// sync.
		return err
	}

	// Remove the check from the local registry
	if cr, ok := c.checkRunners[id]; ok {
		cr.Stop()
		delete(c.checkRunners, id)
	}

	return nil
}

// Run triggers periodic syncing of services and checks with Consul.  This is
// a long lived go-routine which is stopped during shutdown.
func (c *Syncer) Run() {
	sync := time.NewTimer(0)
	for {
		select {
		case <-sync.C:
			d := syncInterval - lib.RandomStagger(syncInterval/syncJitter)
			sync.Reset(d)

			if err := c.SyncServices(); err != nil {
				if c.consulAvailable {
					c.logger.Printf("[DEBUG] consul.syncer: error in syncing: %v", err)
				}
				c.consulAvailable = false
			} else {
				if !c.consulAvailable {
					c.logger.Printf("[DEBUG] consul.syncer: syncs succesful")
				}
				c.consulAvailable = true
			}
		case <-c.notifySyncCh:
			sync.Reset(syncInterval)
		case <-c.shutdownCh:
			c.Shutdown()
		case <-c.notifyShutdownCh:
			sync.Stop()
			c.logger.Printf("[INFO] consul.syncer: shutting down syncer ")
			return
		}
	}
}

// RunHandlers executes each handler (randomly)
func (c *Syncer) RunHandlers() error {
	c.periodicLock.RLock()
	handlers := make(map[string]types.PeriodicCallback, len(c.periodicCallbacks))
	for name, fn := range c.periodicCallbacks {
		handlers[name] = fn
	}
	c.periodicLock.RUnlock()

	var mErr multierror.Error
	for _, fn := range handlers {
		if err := fn(); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	return mErr.ErrorOrNil()
}

// SyncServices sync the services with the Consul Agent
func (c *Syncer) SyncServices() error {
	if err := c.RunHandlers(); err != nil {
		return err
	}
	if err := c.syncServices(); err != nil {
		return err
	}
	if err := c.syncChecks(); err != nil {
		return err
	}

	return nil
}

// filterConsulServices prunes out all the service who were not registered with
// the syncer
func (c *Syncer) filterConsulServices(consulServices map[string]*consul.AgentService) map[consulServiceID]*consul.AgentService {
	localServices := make(map[consulServiceID]*consul.AgentService, len(consulServices))
	c.registryLock.RLock()
	defer c.registryLock.RUnlock()
	for serviceID, service := range consulServices {
		for domain := range c.servicesGroups {
			if strings.HasPrefix(service.ID, fmt.Sprintf("%s-%s", nomadServicePrefix, domain)) {
				localServices[consulServiceID(serviceID)] = service
				break
			}
		}
	}
	return localServices
}

// filterConsulChecks prunes out all the consul checks which do not have
// services with Syncer's idPrefix.
func (c *Syncer) filterConsulChecks(consulChecks map[string]*consul.AgentCheck) map[consulCheckID]*consul.AgentCheck {
	localChecks := make(map[consulCheckID]*consul.AgentCheck, len(consulChecks))
	c.registryLock.RLock()
	defer c.registryLock.RUnlock()
	for checkID, check := range consulChecks {
		for domain := range c.checkGroups {
			if strings.HasPrefix(check.ServiceID, fmt.Sprintf("%s-%s", nomadServicePrefix, domain)) {
				localChecks[consulCheckID(checkID)] = check
				break
			}
		}
	}
	return localChecks
}

// consulPresent indicates whether the Consul Agent is responding
func (c *Syncer) consulPresent() bool {
	_, err := c.client.Agent().Self()
	return err == nil
}

// runCheck runs a check and updates the corresponding ttl check in consul
func (c *Syncer) runCheck(check Check) {
	res := check.Run()
	if res.Duration >= check.Timeout() {
		c.logger.Printf("[DEBUG] consul.syncer: check took time: %v, timeout: %v", res.Duration, check.Timeout())
	}
	state := consul.HealthCritical
	output := res.Output
	switch res.ExitCode {
	case 0:
		state = consul.HealthPassing
	case 1:
		state = consul.HealthWarning
	default:
		state = consul.HealthCritical
	}
	if res.Err != nil {
		state = consul.HealthCritical
		output = res.Err.Error()
	}
	if err := c.client.Agent().UpdateTTL(check.ID(), output, state); err != nil {
		if c.consulAvailable {
			c.logger.Printf("[DEBUG] consul.syncer: check %+q failed, disabling Consul checks until until next successful sync: %v", check.ID(), err)
			c.consulAvailable = false
		} else {
			c.consulAvailable = true
		}
	}
}

// ReapUnmatched prunes all services that do not exist in the passed domains
func (c *Syncer) ReapUnmatched(domains []ServiceDomain) error {
	servicesInConsul, err := c.ConsulClient().Agent().Services()
	if err != nil {
		return err
	}

	var mErr multierror.Error
	for serviceID := range servicesInConsul {
		// Skip any service that was not registered by Nomad
		if !strings.HasPrefix(serviceID, nomadServicePrefix) {
			continue
		}

		// Filter services that do not exist in the desired domains
		match := false
		for _, domain := range domains {
			// Include the hyphen so it is explicit to that domain otherwise it
			// maybe a subset match
			desired := fmt.Sprintf("%s-%s-", nomadServicePrefix, domain)
			if strings.HasPrefix(serviceID, desired) {
				match = true
				break
			}
		}

		if !match {
			if err := c.deregisterService(serviceID); err != nil {
				mErr.Errors = append(mErr.Errors, err)
			}
		}
	}

	return mErr.ErrorOrNil()
}

// AddPeriodicHandler adds a uniquely named callback.  Returns true if
// successful, false if a handler with the same name already exists.
func (c *Syncer) AddPeriodicHandler(name string, fn types.PeriodicCallback) bool {
	c.periodicLock.Lock()
	defer c.periodicLock.Unlock()
	if _, found := c.periodicCallbacks[name]; found {
		c.logger.Printf("[ERROR] consul.syncer: failed adding handler %+q", name)
		return false
	}
	c.periodicCallbacks[name] = fn
	return true
}

// NumHandlers returns the number of callbacks registered with the syncer
func (c *Syncer) NumHandlers() int {
	c.periodicLock.RLock()
	defer c.periodicLock.RUnlock()
	return len(c.periodicCallbacks)
}

// RemovePeriodicHandler removes a handler with a given name.
func (c *Syncer) RemovePeriodicHandler(name string) {
	c.periodicLock.Lock()
	defer c.periodicLock.Unlock()
	delete(c.periodicCallbacks, name)
}

// ConsulClient returns the Consul client used by the Syncer.
func (c *Syncer) ConsulClient() *consul.Client {
	return c.client
}
