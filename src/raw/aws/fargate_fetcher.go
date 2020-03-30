package aws

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	docker "github.com/docker/docker/api/types"
	"github.com/newrelic/infra-integrations-sdk/log"
	"github.com/newrelic/infra-integrations-sdk/persist"

	"github.com/newrelic/nri-docker/src/biz"
)

// FargateStats holds a map of Fargate container IDs as key and their Docker metrics
// as values.
type FargateStats map[string]*docker.Stats

// FargateFetcher fetches metrics from Fargate endpoints in AWS ECS.
type FargateFetcher struct {
	client         *http.Client
	cpuStore       persist.Storer
	containerStore persist.Storer
	logger         log.Logger
	latestFetch    time.Time
}

// NewFargateFetcher creates a new FargateFetcher with the given HTTP client.
func NewFargateFetcher(c *http.Client, l log.Logger) (*FargateFetcher, error) {
	cpuStore, err := persist.NewFileStore(
		persist.DefaultPath("fargate_container_cpus"),
		log.NewStdErr(true),
		60*time.Second)
	if err != nil {
		return nil, err
	}

	containerStore, err := persist.NewFileStore(
		persist.DefaultPath("fargate_container_metadata"),
		log.NewStdErr(true),
		60*time.Second)
	if err != nil {
		return nil, err
	}

	return &FargateFetcher{client: c, cpuStore: cpuStore, containerStore: containerStore, logger: l}, nil
}

// InspectContainer looks up for metadata of a container given a containerID.
func (e *FargateFetcher) InspectContainer(containerID string) (docker.Container, error) {
	defer func() {
		if err := e.containerStore.Save(); err != nil {
			e.logger.Errorf("persisting previous metrics: %v", err)
		}
	}()
	var taskResponse TaskResponse
	var containerResponse ContainerResponse

	// Try to load container from the cache cpuStore.
	_, err := e.containerStore.Get(containerID, &containerResponse)
	if err == nil {
		e.logger.Debugf("found container %s in cache cpuStore", containerID)
		return containerResponseToDocker(containerResponse), nil
	}

	err = e.fetchTaskResponse(&taskResponse)
	if err != nil {
		return docker.Container{}, err
	}

	for _, container := range taskResponse.Containers {
		if container.ID != containerID {
			continue
		}
		e.containerStore.Set(container.ID, container)
		return containerResponseToDocker(container), nil
	}

	return docker.Container{}, nil
}

func (e *FargateFetcher) fetchTaskResponse(taskResponse *TaskResponse) error {
	endpoint, ok := TaskMetadataEndpoint()
	if !ok {
		return errors.New("could not find ECS container metadata URI")
	}

	response, err := metadataResponse(e.client, endpoint)
	if err != nil {
		return fmt.Errorf(
			"error when sending request to ECS task metadata endpoint (%s): %v",
			endpoint,
			err,
		)
	}

	err = json.Unmarshal(response, taskResponse)
	if err != nil {
		return fmt.Errorf("error unmarshalling ECS task: %v", err)
	}
	e.logger.Debugf("task metadata response from %s: %s", endpoint, string(response))
	return nil
}

func containerResponseToDocker(container ContainerResponse) docker.Container {
	c := docker.Container{
		ID:      container.ID,
		Names:   []string{container.Name},
		Image:   container.Image,
		ImageID: container.ImageID,
		Labels:  container.Labels,
		Status:  container.KnownStatus,
	}
	if created := container.CreatedAt; created != nil {
		c.Created = created.Unix()
	}
	return c
}

// GetContainerMetrics returns Docker metrics from inside a Fargate container.
// It captured the ECS container metadata endpoint from the environment variable defined by
// `containerMetadataEnvVar`.
func (e *FargateFetcher) GetContainerMetrics() (*FargateStats, error) {
	var stats FargateStats
	endpoint, ok := TaskStatsEndpoint()
	if !ok {
		return nil, errors.New("could not find ECS container metadata URI")
	}

	response, err := metadataResponse(e.client, endpoint)
	if err != nil {
		return nil, fmt.Errorf(
			"error when sending request to ECS container metadata endpoint (%s): %v",
			endpoint,
			err,
		)
	}
	e.latestFetch = time.Now()

	err = json.Unmarshal(response, &stats)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling ECS container: %v", err)
	}
	e.logger.Debugf("container metrics response from %s: %s", endpoint, string(response))

	return &stats, nil
}

// BizMetricsFromDocker casts metrics from `docker.Stats` into `Biz.Sample`.
func (e *FargateFetcher) BizMetricsFromDocker(containerID string, ds *docker.Stats) biz.Sample {
	var s biz.Sample
	s.Pids = biz.Pids{
		Current: ds.PidsStats.Current,
		Limit:   ds.PidsStats.Limit,
	}

	s.Memory = biz.Memory{
		// We trust the memory usage from Fargate even though the Docker one is not precise.
		UsageBytes:      ds.MemoryStats.Usage,
		CacheUsageBytes: ds.MemoryStats.Stats["cache"],
		RSSUsageBytes:   ds.MemoryStats.Stats["rss"],
		MemLimitBytes:   ds.MemoryStats.Limit,
		UsagePercent:    float64(ds.MemoryStats.Usage / ds.MemoryStats.Limit),
	}

	// Seems like we can't get this information?
	s.RestartCount = 0

	// Requires summing some data structures
	s.BlkIO = blkIOFromDocker(ds.BlkioStats)

	// Requires some calculation over past reading to be able to determine
	// the percentages.
	s.CPU = e.cpuFromDocker(containerID, ds.CPUStats)

	return s
}

var previous struct {
	Time int64
	CPU  docker.CPUStats
}

func (e *FargateFetcher) cpuFromDocker(containerID string, dockerCPU docker.CPUStats) biz.CPU {
	cpu := biz.CPU{}
	// cpuStore current metrics to be the "previous" metrics in the next CPU sampling
	defer func() {
		previous.Time = e.latestFetch.Unix()
		previous.CPU = dockerCPU
		e.cpuStore.Set(containerID, previous)
		if err := e.cpuStore.Save(); err != nil {
			e.logger.Errorf("persisting previous metrics: %v", err)
		}
	}()

	cpu.LimitCores = float64(dockerCPU.OnlineCPUs)

	// Reading previous CPU stats
	if _, err := e.cpuStore.Get(containerID, &previous); err != nil {
		e.logger.Debugf("could not retrieve previous CPU stats for container %v: %v", containerID, err)
		return cpu
	}

	// calculate the change for the cpu usage of the container in between readings
	durationNS := float64(time.Now().Sub(time.Unix(previous.Time, 0)).Nanoseconds())
	if durationNS <= 0 {
		return cpu
	}

	maxVal := float64(len(dockerCPU.CPUUsage.PercpuUsage) * 100)

	cpu.CPUPercent = cpuPercent(previous.CPU, dockerCPU)

	userDelta := float64(dockerCPU.CPUUsage.UsageInUsermode - previous.CPU.CPUUsage.UsageInUsermode)
	cpu.UserPercent = math.Min(maxVal, userDelta*100/durationNS)

	kernelDelta := float64(dockerCPU.CPUUsage.UsageInKernelmode - previous.CPU.CPUUsage.UsageInKernelmode)
	cpu.KernelPercent = math.Min(maxVal, kernelDelta*100/durationNS)

	cpu.UsedCores = float64(dockerCPU.CPUUsage.TotalUsage-previous.CPU.CPUUsage.TotalUsage) / durationNS

	cpu.ThrottlePeriods = dockerCPU.ThrottlingData.ThrottledPeriods
	cpu.ThrottledTimeMS = float64(dockerCPU.ThrottlingData.ThrottledTime) / float64(time.Second)

	cpu.UsedCoresPercent = 100 * cpu.UsedCores / cpu.LimitCores

	return cpu
}

func blkIOFromDocker(io docker.BlkioStats) biz.BlkIO {
	bio := biz.BlkIO{}
	for _, svc := range io.IoServicedRecursive {
		if len(svc.Op) == 0 {
			continue
		}
		switch svc.Op[0] {
		case 'r', 'R':
			bio.TotalReadCount += float64(svc.Value)
		case 'w', 'W':
			bio.TotalWriteCount += float64(svc.Value)
		}
	}
	for _, bytes := range io.IoServiceBytesRecursive {
		if len(bytes.Op) == 0 {
			continue
		}
		switch bytes.Op[0] {
		case 'r', 'R':
			bio.TotalReadBytes += float64(bytes.Value)
		case 'w', 'W':
			bio.TotalWriteBytes += float64(bytes.Value)
		}
	}
	return bio
}

func cpuPercent(previous, current docker.CPUStats) float64 {
	var (
		cpuPercent = 0.0
		// calculate the change for the cpu usage of the container in between readings
		cpuDelta = float64(current.CPUUsage.TotalUsage - previous.CPUUsage.TotalUsage)
		// calculate the change for the entire system between readings
		systemDelta = float64(current.SystemUsage - previous.SystemUsage)
		onlineCPUs  = float64(len(current.CPUUsage.PercpuUsage))
	)

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		cpuPercent = (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}
	return cpuPercent
}
