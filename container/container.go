package container

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"jtso/logger"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type ContainerStats struct {
	Interval int
	Stats    map[string]map[string]float64
	StMu     *sync.Mutex
}

var Cstats *ContainerStats

func Init(i int) {
	Cstats = new(ContainerStats)
	Cstats.Interval = i
	Cstats.Stats = make(map[string]map[string]float64)
	Cstats.StMu = new(sync.Mutex)
}

func calculateCPUPercent(current, previous container.StatsResponse) float64 {
	cpuDelta := float64(current.CPUStats.CPUUsage.TotalUsage - previous.CPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(current.CPUStats.SystemUsage - previous.CPUStats.SystemUsage)
	onlineCPUs := float64(current.CPUStats.OnlineCPUs)

	// Avoid division by zero
	if systemDelta <= 0.0 || onlineCPUs <= 0.0 {
		return 0.0
	}

	// Calculate CPU percentage
	return (cpuDelta / systemDelta) * onlineCPUs * 100.0
}

func collectStats(cli *client.Client, containerSummary container.Summary, resultChan chan<- map[string]map[string]float64, wg *sync.WaitGroup) {
	defer wg.Done()

	// Get initial stats
	stats, err := cli.ContainerStats(context.Background(), containerSummary.ID, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		resultChan <- map[string]map[string]float64{containerSummary.Names[0]: {"error": 1.0}}
		return
	}
	defer stats.Body.Close()

	var prevStats container.StatsResponse
	if err := json.NewDecoder(stats.Body).Decode(&prevStats); err != nil {
		resultChan <- map[string]map[string]float64{containerSummary.Names[0]: {"error": 1.0}}
		return
	}

	// Wait for 1 second
	time.Sleep(time.Duration(Cstats.Interval) * time.Second)

	// Get next stats
	stats, err = cli.ContainerStats(context.Background(), containerSummary.ID, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		resultChan <- map[string]map[string]float64{containerSummary.Names[0]: {"error": 1.0}}
		return
	}
	defer stats.Body.Close()

	var currentStats container.StatsResponse
	if err := json.NewDecoder(stats.Body).Decode(&currentStats); err != nil {
		resultChan <- map[string]map[string]float64{containerSummary.Names[0]: {"error": 1.0}}
		return
	}

	// Calculate CPU percentage
	cpuPercent := calculateCPUPercent(currentStats, prevStats)

	// Calculate memory percentage
	memUsage := float64(currentStats.MemoryStats.Usage)
	// Substract the cache mem
	if cache, ok := currentStats.MemoryStats.Stats["cache"]; ok {
		memUsage -= float64(cache)
	}
	memLimit := float64(currentStats.MemoryStats.Limit)
	memPercent := 0.0
	if memLimit > 0 {
		memPercent = (memUsage / memLimit) * 100.0
	}

	// Format results
	containerName := strings.TrimPrefix(containerSummary.Names[0], "/")
	resultChan <- map[string]map[string]float64{
		containerName: {
			"cpu": cpuPercent,
			"mem": memPercent,
		},
	}
}

func GetContainerLogs(containerName string) ([]string, error) {
	var logLines []string
	logLines = make([]string, 0)

	// Open Docker API
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Unable to open Docker session: %v", err)
		return logLines, err
	}
	defer cli.Close()

	containers, err := cli.ContainerList(context.Background(), client.ContainerListOptions{All: true})
	if err != nil {
		logger.Log.Errorf("Unable to list the containers: %v", err)
		return logLines, err
	}

	var containerID string
	for _, containerSummary := range containers.Items {
		if slices.Contains(containerSummary.Names, "/"+containerName) {
			containerID = containerSummary.ID
			break
		}
	}

	if containerID == "" {
		logger.Log.Errorf("Container with name '%s' not found", containerName)
		return logLines, fmt.Errorf("container with name '%s' not found", containerName)
	}

	ctx := context.Background()
	options := client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       fmt.Sprintf("%d", 200),
	}

	logs, err := cli.ContainerLogs(ctx, containerID, options)
	if err != nil {
		logger.Log.Errorf("Unable to retrieve log for container %s: %v", containerName, err)
		return logLines, err
	}
	defer logs.Close()

	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 8 {
			line = line[8:] // Remove Docker log stream header
		}
		logLines = append(logLines, string(line))
	}

	if err := scanner.Err(); err != nil {
		logger.Log.Errorf("Unexpected error while collecting log fors container %s: %v", containerName, err)
		return logLines, err
	}

	return logLines, nil
}

func GetContainerStats() {
	logger.Log.Debug("Start collecting container stats")

	// Initialize Docker client
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Error creating Docker client: %v\n", err)
		return
	}

	// Get list of containers
	containers, err := cli.ContainerList(context.Background(), client.ContainerListOptions{})
	if err != nil {
		logger.Log.Errorf("Error listing containers: %v\n", err)
		return
	}

	// Set up synchronization
	var wg sync.WaitGroup
	resultChan := make(chan map[string]map[string]float64, len(containers.Items))

	// Collect stats in parallel
	for _, containerSummary := range containers.Items {
		wg.Add(1)
		go collectStats(cli, containerSummary, resultChan, &wg)
	}

	// Wait for all goroutines to finish
	wg.Wait()
	close(resultChan)

	// Aggregate results
	Cstats.StMu.Lock()
	Cstats.Stats = make(map[string]map[string]float64)
	for result := range resultChan {
		maps.Copy(Cstats.Stats, result)
	}
	Cstats.StMu.Unlock()
	logger.Log.Debug("End of the container stats collection")
}

func ListContainers() []container.Summary {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Unable to open Docker session: %v", err)
	}
	defer cli.Close()

	containers, err := cli.ContainerList(context.Background(), client.ContainerListOptions{})
	if err != nil {
		logger.Log.Errorf("Unable to list container state: %v", err)
	}
	logger.Log.Debug("List of containers has been retrieved")
	return containers.Items
}

func RestartContainer(name string) error {
	timeout := 30

	// Open Docker API
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Unable to open Docker session: %v", err)
		return err
	}
	defer cli.Close()

	// Restart container
	_, err = cli.ContainerRestart(context.Background(), name, client.ContainerRestartOptions{Signal: "SIGTERM", Timeout: &timeout})
	if err != nil {
		logger.Log.Errorf("Unable to restart %s container: %v", name, err)
		return err
	}
	logger.Log.Infof("%s container has been restarted", name)
	return nil

}

func StopContainer(name string) {
	timeout := 30

	// Open Docker API
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Unable to open Docker session: %v", err)
		return
	}
	defer cli.Close()

	_, err = cli.ContainerStop(context.Background(), name, client.ContainerStopOptions{Signal: "SIGTERM", Timeout: &timeout})
	if err != nil {
		logger.Log.Errorf("Unable to stop %s container: %v", name, err)
		return
	}
	logger.Log.Infof("%s container has been stopped - no router to collect", name)

}

func GetVersionLabel(name string) string {

	// Open Docker API
	cli, err := client.New(client.FromEnv)
	if err != nil {
		logger.Log.Errorf("Unable to open Docker session: %v", err)
		return "N/A"
	}
	defer cli.Close()

	// Get the image details using the Docker API
	imageInspect, err := cli.ImageInspect(context.Background(), name)
	if err != nil {
		logger.Log.Errorf("Unable to retrieve Docker %s inspect data: %v", name, err)
		return "N/A"

	}

	// Extract the version label from imageInspect.Config.Labels
	version, ok := imageInspect.Config.Labels["version"]
	if !ok {
		logger.Log.Errorf("Unable to retrieve Docker %s version", name)
		return "N/A"

	}

	logger.Log.Debugf("%s container version is %s", name, version)
	return version

}
