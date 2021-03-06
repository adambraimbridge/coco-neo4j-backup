package main

import (
	"github.com/coreos/fleet/client"
	"net/url"
	"net"
	"time"
	"golang.org/x/net/proxy"
	"net/http"
	log "github.com/Sirupsen/logrus"
	"github.com/coreos/fleet/schema"
	"errors"
)

// lifted from the fleet client library for mocking purposes.
type fleetAPI interface {
	UnitStates() ([]*schema.UnitState, error)
	SetUnitTargetState(name, target string) error
}

func newFleetClient(fleetEndpoint string, socksProxy string) (fleetAPI, error) {
	u, err := url.Parse(fleetEndpoint)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100}}

	if socksProxy != "" {
		log.Printf("using SOCKS proxy %s\n", socksProxy)
		netDialler := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialer, err2 := proxy.SOCKS5("tcp", socksProxy, nil, netDialler)
		if err2 != nil {
			log.Fatalf("error with proxy %s: %v\n", socksProxy, err2)
		}
		httpClient.Transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			Dial:                dialer.Dial,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 100,
		}
	}

	log.WithFields(log.Fields{"url": u}).Info("Connecting to fleet API.")
	fleetHTTPAPIClient, err := client.NewHTTPClient(httpClient, *u)
	if err != nil {
		return nil, err
	}
	log.WithFields(log.Fields{"url": u}).Info("Connection successfully established to fleet API.")
	return fleetHTTPAPIClient, err
}

func shutDownNeo(fleetClient fleetAPI) (error) {
	deployerServiceName := "deployer.service"
	isDeployerActive, err := isServiceActive(fleetClient, deployerServiceName)
	if isDeployerActive || err != nil {
		log.WithFields(log.Fields{
			"deployerServiceName": deployerServiceName,
			"isDeployerActive": isDeployerActive,
			"err": err,
		}).Error(`Problem: either the deployer is still active, or there was a problem checking its status.
We cannot complete the backup process in case neo4j is accidentally started up again during backup creation.`)
		if err != nil {
			return err
		} else {
			return errors.New(`Problem: either the deployer is still active, or there was a problem checking its status.
We cannot complete the backup process in case neo4j is accidentally started up again during backup creation.`)
		}

	}
	// TODO use the Go fleet API to shut down neo4j's dependencies (ingesters?).
	serviceName := "neo4j-red@1.service"
	err = setTargetState(fleetClient, serviceName, "inactive")
	return err
	// TODO check whether neo4j has successfully been shut down
}

func setTargetState(fleetClient fleetAPI, serviceName string, targetState string) (error) {
	err := fleetClient.SetUnitTargetState(serviceName, targetState)
	if err != nil {
		log.WithFields(log.Fields{
			"err": err,
			"targetState": targetState,
			"serviceName": serviceName,
		}).Error("Problem setting unit target state!")
	} else {
		log.WithFields(log.Fields{
			"err": err,
			"targetState": targetState,
			"serviceName": serviceName,
		}).Info("Set unit target state successfully.")
	}
	return err
}

func isServiceActive(fleetClient fleetAPI, serviceName string) (bool, error) {
	unitStates, err := fleetClient.UnitStates()
	isActive := false
	found := false
	if err != nil {
		log.Error("Could not retrieve list of units from fleet API, do you need to start a SOCKS proxy?")
		return isActive, err
	}
	log.WithFields(log.Fields{"num": len(unitStates)}).Info("Retrieved services from fleet API.")
	for index, each := range unitStates {
		if each.Name == serviceName {
			found = true
			log.WithFields(log.Fields{
				"index": index,
				"name": each.Name,
				"SystemdActiveState": each.SystemdActiveState,
				"SystemdLoadState": each.SystemdLoadState,
			}).Info("Processing service.")
			if each.SystemdActiveState == "active" {
				isActive = true
			}
			break
		}
	}
	if !found {
		log.WithFields(log.Fields{"serviceName": serviceName}).Warn(
			"Could not find service in list of services, assuming the service is inactive.")
	}
	return isActive, err
}

func startNeo(fleetClient fleetAPI) (error) {
	log.Info("Starting up neo4j...")
	serviceName := "neo4j-red@1.service"
	setTargetState(fleetClient, serviceName, "launched")
	// TODO confirm that neo4j has successfully started up.
	return nil
}

