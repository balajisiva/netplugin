/***
Copyright 2014 Cisco Systems Inc. All rights reserved.

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

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	log "github.com/Sirupsen/logrus"
	"github.com/contiv/netplugin/core"
	"github.com/contiv/netplugin/drivers"
	"github.com/contiv/netplugin/netmaster/intent"
	"github.com/contiv/netplugin/netmaster/master"
	"github.com/contiv/netplugin/netmaster/objApi"
	"github.com/contiv/netplugin/resources"
	"github.com/contiv/netplugin/state"
	"github.com/contiv/netplugin/utils"
	"github.com/gorilla/mux"
	"github.com/hashicorp/consul/api"
)

type cliOpts struct {
	help       bool
	debug      bool
	stateStore string
	storeURL   string
	listenURL  string
}

var flagSet *flag.FlagSet

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTION]...\n", os.Args[0])
	flagSet.PrintDefaults()
}

type daemon struct {
	opts          cliOpts
	apiController *objApi.APIController
}

func initStateDriver(opts *cliOpts) (core.StateDriver, error) {
	var cfg *core.Config

	switch opts.stateStore {
	case utils.EtcdNameStr:
		url := "http://127.0.0.1:4001"
		if opts.storeURL != "" {
			url = opts.storeURL
		}
		etcdCfg := &state.EtcdStateDriverConfig{}
		etcdCfg.Etcd.Machines = []string{url}
		cfg = &core.Config{V: etcdCfg}
	case utils.ConsulNameStr:
		url := "http://127.0.0.1:8500"
		if opts.storeURL != "" {
			url = opts.storeURL
		}
		consulCfg := &state.ConsulStateDriverConfig{}
		consulCfg.Consul = api.Config{Address: url}
		cfg = &core.Config{V: consulCfg}
	default:
		return nil, core.Errorf("Unsupported state-store %q", opts.stateStore)
	}

	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	return utils.NewStateDriver(opts.stateStore, string(cfgBytes))
}

func (d *daemon) parseOpts() error {
	flagSet = flag.NewFlagSet("netm", flag.ExitOnError)
	flagSet.BoolVar(&d.opts.help,
		"help",
		false,
		"prints this message")
	flagSet.BoolVar(&d.opts.debug,
		"debug",
		false,
		"Turn on debugging information")
	flagSet.StringVar(&d.opts.stateStore,
		"state-store",
		utils.EtcdNameStr,
		"State store to use")
	flagSet.StringVar(&d.opts.storeURL,
		"store-url",
		"",
		"Etcd or Consul cluster url. Empty string resolves to respective state-store's default URL.")
	flagSet.StringVar(&d.opts.listenURL,
		"listen-url",
		":9999",
		"Url to listen http requests on")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return err
	}

	return nil
}

func (d *daemon) execOpts() {
	if err := d.parseOpts(); err != nil {
		log.Fatalf("Failed to parse cli options. Error: %s", err)
	}

	if d.opts.help {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTION]...\n", os.Args[0])
		flagSet.PrintDefaults()
		os.Exit(0)
	}

	if d.opts.debug {
		log.SetLevel(log.DebugLevel)
	}

	sd, err := initStateDriver(&d.opts)
	if err != nil {
		log.Fatalf("Failed to init state-store. Error: %s", err)
	}

	if _, err = resources.NewStateResourceManager(sd); err != nil {
		log.Fatalf("Failed to init resource manager. Error: %s", err)
	}
}

func (d *daemon) ListenAndServe() {
	r := mux.NewRouter()

	// Create a new api controller
	d.apiController = objApi.NewAPIController(r)

	// Add REST routes
	s := r.Headers("Content-Type", "application/json").Methods("Post").Subrouter()
	s.HandleFunc(fmt.Sprintf("/%s", master.DesiredConfigRESTEndpoint),
		post(d.desiredConfig))
	s.HandleFunc(fmt.Sprintf("/%s", master.AddConfigRESTEndpoint),
		post(d.addConfig))
	s.HandleFunc(fmt.Sprintf("/%s", master.DelConfigRESTEndpoint),
		post(d.delConfig))
	s.HandleFunc(fmt.Sprintf("/%s", master.HostBindingConfigRESTEndpoint),
		post(d.hostBindingsConfig))

	s = r.Methods("Get").Subrouter()
	s.HandleFunc(fmt.Sprintf("/%s/%s", master.GetEndpointRESTEndpoint, "{id}"),
		get(false, d.endpoints))
	s.HandleFunc(fmt.Sprintf("/%s", master.GetEndpointsRESTEndpoint),
		get(true, d.endpoints))
	s.HandleFunc(fmt.Sprintf("/%s/%s", master.GetNetworkRESTEndpoint, "{id}"),
		get(false, d.networks))
	s.HandleFunc(fmt.Sprintf("/%s", master.GetNetworksRESTEndpoint),
		get(true, d.networks))
	if err := http.ListenAndServe(d.opts.listenURL, r); err != nil {
		log.Fatalf("Error listening for http requests. Error: %s", err)
	}
}

func post(hook func(cfg *intent.Config) error) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := &intent.Config{}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(cfg); err != nil {
			http.Error(w,
				core.Errorf("parsing json failed. Error: %s", err).Error(),
				http.StatusInternalServerError)
			return
		}

		if err := hook(cfg); err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		return
	}
}

func (d *daemon) desiredConfig(cfg *intent.Config) error {
	if err := master.DeleteDelta(cfg); err != nil {
		return err
	}

	if err := master.ProcessAdditions(cfg); err != nil {
		return err
	}
	return nil
}

func (d *daemon) addConfig(cfg *intent.Config) error {
	if err := master.ProcessAdditions(cfg); err != nil {
		return err
	}
	return nil
}

func (d *daemon) delConfig(cfg *intent.Config) error {
	if err := master.ProcessDeletions(cfg); err != nil {
		return err
	}
	return nil
}

func (d *daemon) hostBindingsConfig(cfg *intent.Config) error {
	if err := master.CreateEpBindings(&cfg.HostBindings); err != nil {
		return err
	}
	return nil
}

func get(getAll bool, hook func(id string) ([]core.State, error)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			idStr  string
			states []core.State
			resp   []byte
			ok     bool
			err    error
		)

		if getAll {
			idStr = "all"
		} else if idStr, ok = mux.Vars(r)["id"]; !ok {
			http.Error(w,
				core.Errorf("Failed to find the id string in the request.").Error(),
				http.StatusInternalServerError)
		}

		if states, err = hook(idStr); err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
			return
		}

		if resp, err = json.Marshal(states); err != nil {
			http.Error(w,
				core.Errorf("marshalling json failed. Error: %s", err).Error(),
				http.StatusInternalServerError)
			return
		}

		w.Write(resp)
		return
	}
}

// XXX: This function should be returning logical state instead of driver state
func (d *daemon) endpoints(id string) ([]core.State, error) {
	var (
		err error
		ep  *drivers.OvsOperEndpointState
	)

	ep = &drivers.OvsOperEndpointState{}
	if ep.StateDriver, err = utils.GetStateDriver(); err != nil {
		return nil, err
	}

	if id == "all" {
		return ep.ReadAll()
	}

	err = ep.Read(id)
	if err == nil {
		return []core.State{core.State(ep)}, nil
	}

	return nil, core.Errorf("Unexpected code path. Recieved error during read: %v", err)
}

// XXX: This function should be returning logical state instead of driver state
func (d *daemon) networks(id string) ([]core.State, error) {
	var (
		err error
		nw  *drivers.OvsCfgNetworkState
	)

	nw = &drivers.OvsCfgNetworkState{}
	if nw.StateDriver, err = utils.GetStateDriver(); err != nil {
		return nil, err
	}

	if id == "all" {
		return nw.ReadAll()
	} else if err := nw.Read(id); err == nil {
		return []core.State{core.State(nw)}, nil
	}

	return nil, core.Errorf("Unexpected code path")
}

func main() {
	d := &daemon{}
	d.execOpts()
	d.ListenAndServe()
}
