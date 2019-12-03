/*
Copyright 2019 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"os"
	"time"

	"github.com/pkg/errors"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"

	// Allow auth to cloud providers
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	syncPeriod   = "30s"
	errCleanup   = "failure in default cleanup"
	errGetRemote = "unable to download CRDs from remote location"
	remotePath   = "./tmp-test"
)

// OperationFn is a function that uses a Kubernetes client to perform and
// operation
type OperationFn func(*envtest.Environment, client.Client) error

// Config is a set of configuration values for setup.
type Config struct {
	RemoteCRDPaths    []string
	CRDDirectoryPaths []string
	Builder           OperationFn
	Cleaners          []OperationFn
	ManagerOptions    manager.Options
}

// NewBuilder returns a new no-op Builder
func NewBuilder() OperationFn {
	return func(*envtest.Environment, client.Client) error {
		return nil
	}
}

// NewCRDCleaner returns a new Cleaner that deletes all installed CRDs from the
// API server.
func NewCRDCleaner() OperationFn {
	return func(e *envtest.Environment, c client.Client) error {
		cs, err := clientset.NewForConfig(e.Config)
		if err != nil {
			return errors.Wrap(err, errCleanup)
		}
		var crds []*apiextensionsv1beta1.CustomResourceDefinition
		for _, path := range e.CRDDirectoryPaths {
			crd, err := readCRDs(path)
			if err != nil {
				return errors.Wrap(err, errCleanup)
			}
			crds = append(crds, crd...)
		}

		for _, crd := range crds {
			if err := cs.ApiextensionsV1beta1().CustomResourceDefinitions().Delete(crd.Name, nil); err != nil {
				return errors.Wrap(err, errCleanup)
			}
		}
		return nil
	}
}

// NewRemoteDirCleaner cleans up the default directory where remote CRDs were
// downloaded.
func NewRemoteDirCleaner() OperationFn {
	return func(e *envtest.Environment, c client.Client) error {
		return os.RemoveAll(remotePath)
	}
}

// An Option configures a Config.
type Option func(*Config)

// WithBuilder sets a custom builder function for a Config.
func WithBuilder(builder OperationFn) Option {
	return func(c *Config) {
		c.Builder = builder
	}
}

// WithCleaners sets custom cleaner functios for a Config.
func WithCleaners(cleaners ...OperationFn) Option {
	return func(c *Config) {
		c.Cleaners = cleaners
	}
}

// WithCRDDirectoryPaths sets custom CRD locations for a Config.
func WithCRDDirectoryPaths(crds ...string) Option {
	return func(c *Config) {
		c.CRDDirectoryPaths = crds
	}
}

// WithRemoteCRDPaths sets custom remote CRD locations for a Config.
func WithRemoteCRDPaths(urls ...string) Option {
	return func(c *Config) {
		c.RemoteCRDPaths = urls
	}
}

// WithManagerOptions sets custom options for the manager configured by
// Config.
func WithManagerOptions(m manager.Options) Option {
	return func(c *Config) {
		c.ManagerOptions = m
	}
}

func defaultConfig() *Config {
	t, err := time.ParseDuration(syncPeriod)
	if err != nil {
		panic(err)
	}

	return &Config{
		RemoteCRDPaths:    []string{},
		CRDDirectoryPaths: []string{},
		Builder:           NewBuilder(),
		Cleaners:          []OperationFn{NewCRDCleaner()},
		ManagerOptions:    manager.Options{SyncPeriod: &t},
	}
}

// Manager wraps a controller-runtime manager with additional functionality.
type Manager struct {
	manager.Manager
	stop   chan struct{}
	env    *envtest.Environment
	client client.Client
	c      *Config
}

// New creates a new Manager.
func New(cfg *rest.Config, o ...Option) (*Manager, error) {
	var useExisting bool
	if cfg != nil {
		useExisting = true
	}

	c := defaultConfig()
	for _, op := range o {
		op(c)
	}

	for _, url := range c.RemoteCRDPaths {
		if err := downloadPath(url, remotePath); err != nil {
			return nil, errors.Wrap(err, errGetRemote)
		}
	}

	c.CRDDirectoryPaths = append(c.CRDDirectoryPaths, remotePath)

	e := &envtest.Environment{
		CRDDirectoryPaths:  c.CRDDirectoryPaths,
		Config:             cfg,
		UseExistingCluster: &useExisting,
	}

	cfg, err := e.Start()
	if err != nil {
		return nil, err
	}

	client, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, err
	}

	if err := c.Builder(e, client); err != nil {
		return nil, err
	}

	mgr, err := manager.New(cfg, c.ManagerOptions)
	if err != nil {
		return nil, err
	}

	stop := make(chan struct{})
	return &Manager{mgr, stop, e, client, c}, nil
}

// Run starts a controller-runtime manager with a signal channel.
func (m *Manager) Run() {
	go func() {
		if err := m.Start(m.stop); err != nil {
			panic(err)
		}
	}()
}

// GetClient returns a Kubernetes rest client.
func (m *Manager) GetClient() client.Client {
	return m.client
}

// Cleanup runs the supplied cleanup or defaults to deleting all CRDs.
func (m *Manager) Cleanup() error {
	close(m.stop)
	for _, clean := range m.c.Cleaners {
		if err := clean(m.env, m.client); err != nil {
			return err
		}
	}

	return m.env.Stop()
}
