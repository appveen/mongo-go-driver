// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mtest

import (
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/appveen/mongo-go-driver/bson"
	"github.com/appveen/mongo-go-driver/mongo"
	"github.com/appveen/mongo-go-driver/mongo/options"
	"github.com/appveen/mongo-go-driver/mongo/readpref"
	"github.com/appveen/mongo-go-driver/mongo/writeconcern"
	"github.com/appveen/mongo-go-driver/x/mongo/driver/connstring"
	"github.com/appveen/mongo-go-driver/x/mongo/driver/description"
	"github.com/appveen/mongo-go-driver/x/mongo/driver/topology"
)

const (
	// TestDb specifies the name of default test database.
	TestDb = "test"
)

// testContext holds the global context for the integration tests. The testContext members should only be initialized
// once during the global setup in TestMain. These variables should only be accessed indirectly through MongoTest
// instances.
var testContext struct {
	connString    connstring.ConnString
	topo          *topology.Topology
	topoKind      TopologyKind
	client        *mongo.Client // client used for setup and teardown
	serverVersion string
	authEnabled   bool
}

func setupClient(cs connstring.ConnString, opts *options.ClientOptions) (*mongo.Client, error) {
	wcMajority := writeconcern.New(writeconcern.WMajority())
	return mongo.Connect(Background, opts.ApplyURI(cs.Original).SetWriteConcern(wcMajority))
}

// Setup initializes the current testing context.
// This function must only be called one time and must be called before any tests run.
func Setup() error {
	var err error
	testContext.connString, err = getConnString()
	if err != nil {
		log.Printf("connstring error: %v", err)
		return err
	}
	testContext.topo, err = topology.New(topology.WithConnString(func(connstring.ConnString) connstring.ConnString {
		return testContext.connString
	}))
	if err != nil {
		log.Printf("topology.New error: %v", err)
		return err
	}
	if err = testContext.topo.Connect(); err != nil {
		log.Printf("topology.Connect error: %v", err)
		return err
	}

	testContext.client, err = setupClient(testContext.connString, options.Client())
	if err != nil {
		log.Printf("error setting up client: %v", err)
		return err
	}
	if err := testContext.client.Ping(Background, readpref.Primary()); err != nil {
		log.Printf("error connecting to client: %v", err)
		return err
	}

	if testContext.serverVersion, err = getServerVersion(); err != nil {
		log.Printf("error getting server version: %v", err)
		return err
	}

	switch testContext.topo.Kind() {
	case description.Single:
		testContext.topoKind = Single
	case description.ReplicaSet, description.ReplicaSetWithPrimary, description.ReplicaSetNoPrimary:
		testContext.topoKind = ReplicaSet
	case description.Sharded:
		testContext.topoKind = Sharded
	}

	if testContext.topoKind == ReplicaSet && compareVersions(testContext.serverVersion, "4.0") >= 0 {
		err = testContext.client.Database("admin").RunCommand(Background, bson.D{
			{"setParameter", 1},
			{"transactionLifetimeLimitSeconds", 3},
		}).Err()
		if err != nil {
			return err
		}
	}

	testContext.authEnabled = len(os.Getenv("MONGO_GO_DRIVER_CA_FILE")) != 0
	return nil
}

// Teardown cleans up resources initialized by Setup.
// This function must be called once after all tests have finished running.
func Teardown() error {
	if err := testContext.client.Database(TestDb).Drop(Background); err != nil {
		log.Printf("Database.Drop error: %v", err)
		return err
	}
	if err := testContext.client.Disconnect(Background); err != nil {
		log.Printf("Client.Disconnect error: %v", err)
		return err
	}
	if err := testContext.topo.Disconnect(Background); err != nil {
		log.Printf("Topology.Disconnect error: %v", err)
		return err
	}
	return nil
}

func getServerVersion() (string, error) {
	var serverStatus bson.Raw
	err := testContext.client.Database(TestDb).RunCommand(
		Background,
		bson.D{{"serverStatus", 1}},
	).Decode(&serverStatus)
	if err != nil {
		return "", err
	}

	version, err := serverStatus.LookupErr("version")
	if err != nil {
		return "", errors.New("no version string in serverStatus response")
	}

	return version.StringValue(), nil
}

// addOptions appends connection string options to a URI.
func addOptions(uri string, opts ...string) string {
	if !strings.ContainsRune(uri, '?') {
		if uri[len(uri)-1] != '/' {
			uri += "/"
		}

		uri += "?"
	} else {
		uri += "&"
	}

	for _, opt := range opts {
		uri += opt
	}

	return uri
}

// addTLSConfig checks for the environmental variable indicating that the tests are being run
// on an SSL-enabled server, and if so, returns a new URI with the necessary configuration.
func addTLSConfig(uri string) string {
	caFile := os.Getenv("MONGO_GO_DRIVER_CA_FILE")
	if len(caFile) == 0 {
		return uri
	}

	return addOptions(uri, "ssl=true&sslCertificateAuthorityFile=", caFile)
}

// addCompressors checks for the environment variable indicating that the tests are being run with compression
// enabled. If so, it returns a new URI with the necessary configuration
func addCompressors(uri string) string {
	comp := os.Getenv("MONGO_GO_DRIVER_COMPRESSOR")
	if len(comp) == 0 {
		return uri
	}

	return addOptions(uri, "compressors=", comp)
}

// ConnString gets the globally configured connection string.
func getConnString() (connstring.ConnString, error) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	uri = addTLSConfig(uri)
	uri = addCompressors(uri)
	return connstring.Parse(uri)
}

// compareVersions compares two version number strings (i.e. positive integers separated by
// periods). Comparisons are done to the lesser precision of the two versions. For example, 3.2 is
// considered equal to 3.2.11, whereas 3.2.0 is considered less than 3.2.11.
//
// Returns a positive int if version1 is greater than version2, a negative int if version1 is less
// than version2, and 0 if version1 is equal to version2.
func compareVersions(v1 string, v2 string) int {
	n1 := strings.Split(v1, ".")
	n2 := strings.Split(v2, ".")

	for i := 0; i < int(math.Min(float64(len(n1)), float64(len(n2)))); i++ {
		i1, err := strconv.Atoi(n1[i])
		if err != nil {
			return 1
		}

		i2, err := strconv.Atoi(n2[i])
		if err != nil {
			return -1
		}

		difference := i1 - i2
		if difference != 0 {
			return difference
		}
	}

	return 0
}
