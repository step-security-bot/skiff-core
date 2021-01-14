package setup

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/paralin/skiff-core/config"
	"github.com/paralin/skiff-core/util/multiwriter"
	log "github.com/sirupsen/logrus"
)

// ContainerSetup sets up a container.
type ContainerSetup struct {
	config *config.ConfigContainer
	waiter ImageWaiter
	logger multiwriter.MultiWriter

	wg          sync.WaitGroup
	err         error
	containerId string
}

// NewContainerSetup creates a new ContainerSetup.
func NewContainerSetup(config *config.ConfigContainer, waiter ImageWaiter) *ContainerSetup {
	return &ContainerSetup{config: config, waiter: waiter}
}

// buildDockerContainer builds the Docker API container representation of this config.
func (cs *ContainerSetup) buildDockerContainer() *types.ContainerCreateConfig {
	res := &types.ContainerCreateConfig{Name: cs.config.Name()}

	config := cs.config
	containerConfig := &container.Config{
		Cmd:        config.Cmd,
		Entrypoint: config.Entrypoint,
		Image:      config.Image,
		Tty:        config.Tty,
		WorkingDir: config.WorkingDirectory,
		StopSignal: config.StopSignal,
	}
	res.Config = containerConfig
	for _, ev := range config.Env {
		if ev.Name != "" {
			containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s:%s", ev.Name, ev.Value))
		}
	}
	useInit := !config.DisableInit
	hostConfig := &container.HostConfig{
		CapAdd:      config.CapAdd,
		DNS:         config.DNS,
		DNSSearch:   config.DNSSearch,
		ExtraHosts:  config.Hosts,
		Init:        &useInit,
		SecurityOpt: config.SecurityOpt,
		Tmpfs:       config.TmpFs,
		Privileged:  config.Privileged,
	}
	if rp := config.RestartPolicy; rp != "" {
		hostConfig.RestartPolicy = container.RestartPolicy{
			Name: rp,
		}
	}
	res.HostConfig = hostConfig
	if len(config.Mounts) > 0 {
		hostConfig.Binds = make([]string, len(config.Mounts))
		copy(hostConfig.Binds, config.Mounts)
	}
	if useInit {
		hostConfig.Binds = append(hostConfig.Binds, "/usr/bin/tini:/dev/init")
	}
	if config.HostNetwork {
		hostConfig.NetworkMode = container.NetworkMode("host")
	}
	if config.HostIPC {
		hostConfig.IpcMode = container.IpcMode("host")
	}
	if config.HostPID {
		hostConfig.PidMode = container.PidMode("host")
	}
	if config.HostUTS {
		hostConfig.UTSMode = container.UTSMode("host")
	}

	return res
}

// Execute starts the container setup.
func (cs *ContainerSetup) Execute() (execError error) {
	cs.wg.Add(1)
	defer func() {
		cs.err = execError
		cs.wg.Done()
	}()

	config := cs.config
	if config.Image == "" {
		return fmt.Errorf("Container %s must have image specified.", config.Name())
	}

	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// wait for the image to be ready
	if err := cs.waiter.WaitForImage(config.Image, &cs.logger); err != nil {
		return err
	}

	// check if the container exists
	/*
		filters := filters.NewArgs()
		filters.Add("name", config.Name())
	*/
	list, err := dockerClient.ContainerList(context.Background(), types.ContainerListOptions{
		All: true,
		// Filters: filters,
	})
	if err != nil {
		return err
	}

	le := log.WithField("name", config.Name())
	for _, ctr := range list {
		for _, name := range ctr.Names {
			if name == config.Name() {
				le.Debug("Container already exists")
				cs.containerId = ctr.ID
				return nil
			}
		}
	}

	// create the container
	cconf := cs.buildDockerContainer()
	res, err := dockerClient.ContainerCreate(
		context.Background(),
		cconf.Config,
		cconf.HostConfig,
		cconf.NetworkingConfig,
		cconf.Name,
	)
	if err != nil {
		return err
	}
	le.WithField("id", res.ID).Debug("Container created")
	for _, warning := range res.Warnings {
		le.Warnf("Docker issued warning: %s", warning)
	}
	cs.containerId = res.ID

	cs.logger.Write([]byte("Container created with ID: "))
	cs.logger.Write([]byte(res.ID))
	cs.logger.Write([]byte("\n"))

	if cs.config.StartAfterCreate {
		cs.logger.Write([]byte("Starting container" + res.ID + "...\n"))
		err = dockerClient.ContainerStart(context.Background(), res.ID, types.ContainerStartOptions{})
		if err != nil {
			cs.logger.Write([]byte("Could not start container, continuing: " + err.Error() + "\n"))
		}
	}

	return nil
}

// Wait waits for Execute() to finish.
func (i *ContainerSetup) Wait(log io.Writer) error {
	i.logger.AddWriter(log)
	defer i.logger.RmWriter(log)

	i.wg.Wait()
	return i.err
}

// WaitWithId waits for Execute() to finish and returns the container ID.
func (i *ContainerSetup) WaitWithId(outw io.Writer) (string, error) {
	i.logger.AddWriter(outw)
	defer i.logger.RmWriter(outw)

	i.wg.Wait()
	return i.containerId, i.err
}
