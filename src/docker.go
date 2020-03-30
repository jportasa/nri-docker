package main

import (
	"os"

	"github.com/newrelic/infra-integrations-sdk/integration"
	"github.com/newrelic/infra-integrations-sdk/log"
	"github.com/newrelic/nri-docker/src/nri"
)

type argumentList struct {
	Verbose    bool   `default:"false" help:"Print more information to logs."`
	Pretty     bool   `default:"false" help:"Print pretty formatted JSON."`
	NriCluster string `default:"" help:"Optional. Cluster name"`
	HostRoot   string `default:"/host" help:"If the integration is running from a container, the mounted folder pointing to the host root folder"`
	CgroupPath string `default:"" help:"Optional. The path where cgroup is mounted."`
	Fargate    bool   `default:"false" help:"Enables Fargate container metrics fetching. If enabled no metrics are collected from cgroups or Docker. Defaults to false"`
}

const (
	integrationName    = "com.newrelic.docker"
	integrationVersion = "0.6.0"
)

var (
	args argumentList
)

// Sampler abstracts away different types that can sample that through an integration.Integration instance.
type Sampler interface {
	SampleAll(*integration.Integration) error
}

func main() {
	// Create Integration
	i, err := integration.New(integrationName, integrationVersion, integration.Args(&args))
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	log.SetupLogging(args.Verbose)

	var sampler Sampler

	if args.Fargate {
		sampler = &nri.FargateSampler{}
	} else {
		sampler, err = nri.NewSampler(args.HostRoot, args.CgroupPath)
		exitOnErr(err)
	}
	exitOnErr(sampler.SampleAll(i))
	exitOnErr(i.Publish())
}

func exitOnErr(err error) {
	if err != nil {
		log.Error(err.Error())
		os.Exit(-1)
	}
}
