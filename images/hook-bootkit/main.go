package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/go-logr/logr"
	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
)

type tinkWorkerConfig struct {
	// Registry configuration
	registry string
	username string
	password string

	// Tink Server GRPC address:port
	grpcAuthority string

	// Worker ID
	workerID string

	// tinkWorkerImage is the Tink worker image location.
	tinkWorkerImage string

	// tinkServerTLS is whether or not to use TLS for tink-server communication.
	tinkServerTLS string

	// tinkServerInsecureTLS is whether or not to use insecure TLS for tink-server communication; only applies is TLS itself is on
	tinkServerInsecureTLS string

	httpProxy  string
	httpsProxy string
	noProxy    string
}

func main() {
	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer done()
	log := defaultLogger("debug")
	log.Info("starting BootKit: the tink-worker bootstrapper")

	for {
		if err := run(ctx, log); err != nil {
			log.Error(err, "bootstrapping tink-worker failed")
			log.Info("will retry in 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	log.Info("BootKit: the tink-worker bootstrapper finished")
}

// TODO(jacobweinstock): clean up func run().
// 1. read /proc/cmdline
// 2. parse and populate tinkConfig from contents of /proc/cmdline
// 3. do validation/sanitization on tinkConfig
// 4. setup docker client
// 4. configure any registry auth
// 5. pull tink-worker image
// 6. remove any existing tink-worker container
// 7. setup tink-worker container config
// 8. create tink-worker container
// 9. start tink-worker container
// 10. check that the tink-worker container is running

func run(ctx context.Context, log logr.Logger) error {
	content, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	cmdLines := strings.Split(string(content), " ")
	cfgs := parseCmdLine(cmdLines)
	var runErr error
	// Generate the path to the tink-worker
	// loop through each configs to see if the tink server responds on any one
	for idx, cfg := range cfgs {
		var imageName string
		if cfg.registry != "" {
			imageName = path.Join(cfg.registry, "tink-worker:latest")
		}
		if cfg.tinkWorkerImage != "" {
			imageName = cfg.tinkWorkerImage
		}
		if imageName == "" {
			return fmt.Errorf("cannot pull image for tink-worker, 'docker_registry' and/or 'tink_worker_image' NOT specified in /proc/cmdline")
		}

		// Give time for Docker to start
		// Alternatively we watch for the socket being created
		log.Info("setting up the Docker client")

		os.Setenv("HTTP_PROXY", cfg.httpProxy)
		os.Setenv("HTTPS_PROXY", cfg.httpsProxy)
		os.Setenv("NO_PROXY", cfg.noProxy)
		// Create Docker client with API (socket)
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return err
		}

		log.Info("Pulling image", "imageName", imageName)
		authConfig := registry.AuthConfig{
			Username: cfg.username,
			Password: strings.TrimSuffix(cfg.password, "\n"),
		}

		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}

		authStr := base64.URLEncoding.EncodeToString(encodedJSON)

		pullOpts := image.PullOptions{
			RegistryAuth: authStr,
		}

		var out io.ReadCloser
		imagePullOperation := func() error {
			// with embedded images, the tink worker could potentially already exist
			// in the local Docker image cache. And the image name could be something
			// unreachable via the network (for example: 127.0.0.1/embedded/tink-worker).
			// Because of this we check if the image already exists and don't return an
			// error if the image does not exist and the pull fails.
			var imageExists bool
			if _, _, err := cli.ImageInspectWithRaw(ctx, imageName); err == nil {
				imageExists = true
			}
			out, err = cli.ImagePull(ctx, imageName, pullOpts)
			if err != nil && !imageExists {
				log.Error(err, "image pull failure", "imageName", imageName)
				return err
			}
			return nil
		}
		if err := backoff.Retry(imagePullOperation, backoff.NewExponentialBackOff()); err != nil {
			return err
		}

		if out != nil {
			buf := bufio.NewScanner(out)
			for buf.Scan() {
				structured := make(map[string]interface{})
				if err := json.Unmarshal(buf.Bytes(), &structured); err != nil {
					log.Info("image pull logs", "output", buf.Text())
				} else {
					log.Info("image pull logs", "logs", structured)
				}

			}
			if err := out.Close(); err != nil {
				log.Error(err, "closing image pull logs failed")
			}
		}
		log.Info("Removing any existing tink-worker container")
		if err := removeTinkWorkerContainer(ctx, cli); err != nil {
			return fmt.Errorf("failed to remove existing tink-worker container: %w", err)
		}

		log.Info("Creating tink-worker container")
		tinkContainer := &container.Config{
			Image: imageName,
			Env: []string{
				fmt.Sprintf("DOCKER_REGISTRY=%s", cfg.registry),
				fmt.Sprintf("REGISTRY_USERNAME=%s", cfg.username),
				fmt.Sprintf("REGISTRY_PASSWORD=%s", cfg.password),
				fmt.Sprintf("TINKERBELL_GRPC_AUTHORITY=%s", cfg.grpcAuthority),
				fmt.Sprintf("TINKERBELL_TLS=%s", cfg.tinkServerTLS),
				fmt.Sprintf("TINKERBELL_INSECURE_TLS=%s", cfg.tinkServerInsecureTLS),
				fmt.Sprintf("WORKER_ID=%s", cfg.workerID),
				fmt.Sprintf("ID=%s", cfg.workerID),
				fmt.Sprintf("HTTP_PROXY=%s", cfg.httpProxy),
				fmt.Sprintf("HTTPS_PROXY=%s", cfg.httpsProxy),
				fmt.Sprintf("NO_PROXY=%s", cfg.noProxy),
			},
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
		}
		tinkHostConfig := &container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: "/worker",
					Target: "/worker",
				},
				{
					Type:   mount.TypeBind,
					Source: "/var/run/docker.sock",
					Target: "/var/run/docker.sock",
				},
			},
			NetworkMode: "host",
			Privileged:  true,
		}
		resp, err := cli.ContainerCreate(ctx, tinkContainer, tinkHostConfig, nil, nil, fmt.Sprintf("tink-worker-%d", idx))
		if err != nil {
			return fmt.Errorf("creating tink-worker container failed: %w", err)
		}

		log.Info("Starting tink-worker container")
		if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			return fmt.Errorf("starting tink-worker container failed: %w", err)
		}

		time.Sleep(time.Second * 3)
		// if tink-worker is not running return error so we try again
		if err := checkContainerRunning(ctx, cli, resp.ID); err != nil {
			log.Info("checking if tink-worker container is running failed", "tink-server", cfg.grpcAuthority, "error", err)
			runErr = err
		} else {
			// If the tink-worker starts running then break as we need not check any further
			log.Info("tink-worker container running", "tink-server", cfg.grpcAuthority)
			runErr = nil
		}
		var tinkServerInvalid bool
		log.Info("just entering the tink-server check")
		for range 10 {
			tinkServerInvalid, err = checkContainerLogs(ctx, log, cli, resp.ID)
			if err != nil {
				log.Info("checking if tink-server is active", "tink-server", cfg.grpcAuthority, "error", err)
			}
			if tinkServerInvalid {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !tinkServerInvalid {
			log.Info("tink-server is active won't for further hosts", "tink-server", cfg.grpcAuthority, "error", err)
			break
		} else {
			log.Info("tink-server is inactive will try for further hosts", "tink-server", cfg.grpcAuthority, "error", err)
		}
	}

	return runErr
}

// checkContainerRunning checks if the tink-worker container is running.
func checkContainerRunning(ctx context.Context, cli *client.Client, containerID string) error {
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return err
	}
	if !inspect.State.Running {
		return fmt.Errorf("tink-worker container is not running")
	}
	return nil
}

// checkContainerLogs checks if the provided tink-server is responding on the given address.
func checkContainerLogs(ctx context.Context, log logr.Logger, cli *client.Client, containerID string) (bool, error) {
	reader, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
		Tail:       "40",
	})
	if err != nil {
		log.Info("nothing to read, error fetching logs")
		return false, err
	}
	defer reader.Close()

	// read the first 8 bytes to ignore the HEADER part from docker container logs
	scanner := bufio.NewScanner(reader)
	// p := make([]byte, 8)
	// reader.Read(p)
	var tinkServerInactive bool
	for scanner.Scan() {
		line := scanner.Text()
		log.Info("verbose output of worker logs", "content", line)
		if strings.Contains(line, "502 (Bad Gateway)") || strings.Contains(line, "no route to host") {
			tinkServerInactive = true
			break
		}
	}
	// content, err := io.ReadAll(reader)
	// if err != nil {
	// 	log.Info("error reading the bytes")
	// 	return false, err
	// }
	// log.Info("verbose output of worker logs", "content", string(content))

	return tinkServerInactive, nil
}

// removeTinkWorkerContainer removes the tink-worker container if it exists.
func removeTinkWorkerContainer(ctx context.Context, cli *client.Client) error {
	cs, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("listing containers, in order to find an existing tink-worker container, failed: %w", err)
	}
	for _, c := range cs {
		for _, n := range c.Names {
			if strings.Contains(n, "/tink-worker") {
				if err := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
					return fmt.Errorf("removing existing tink-worker container failed: %w", err)
				}
			}
		}
	}
	return nil
}

// parseCmdLine will parse the command line.
// These values follow what Boots sends to the auto.ipxe Script.
// https://github.com/tinkerbell/boots/blob/main/ipxe/hook.go
func parseCmdLine(cmdLines []string) (cfgs []tinkWorkerConfig) {
	firstCfg := tinkWorkerConfig{}
	secondCfg := tinkWorkerConfig{}
	for i := range cmdLines {
		cmdLine := strings.SplitN(cmdLines[i], "=", 2)
		if len(cmdLine) == 0 {
			continue
		}

		switch cmd := cmdLine[0]; cmd {
		case "docker_registry":
			firstCfg.registry = cmdLine[1]
			secondCfg.registry = cmdLine[1]
		case "registry_username":
			firstCfg.username = cmdLine[1]
			secondCfg.username = cmdLine[1]
		case "registry_password":
			firstCfg.password = cmdLine[1]
			secondCfg.password = cmdLine[1]
		// Provide Tink server IPs on command line as string slice and for POC
		// assume there will be only two separated by ','.
		case "grpc_authority":
			serverIPs := strings.Split(cmdLine[1], ",")
			firstCfg.grpcAuthority = serverIPs[0]
			secondCfg.grpcAuthority = serverIPs[1]
		case "worker_id":
			firstCfg.workerID = cmdLine[1]
			secondCfg.workerID = cmdLine[1]
		case "tink_worker_image":
			firstCfg.tinkWorkerImage = cmdLine[1]
			secondCfg.tinkWorkerImage = cmdLine[1]
		case "tinkerbell_tls":
			firstCfg.tinkServerTLS = cmdLine[1]
			secondCfg.tinkServerTLS = cmdLine[1]
		case "tinkerbell_insecure_tls":
			firstCfg.tinkServerInsecureTLS = cmdLine[1]
			secondCfg.tinkServerInsecureTLS = cmdLine[1]
		case "HTTP_PROXY":
			firstCfg.httpProxy = cmdLine[1]
			secondCfg.httpProxy = cmdLine[1]
		case "HTTPS_PROXY":
			firstCfg.httpsProxy = cmdLine[1]
			secondCfg.httpsProxy = cmdLine[1]
		case "NO_PROXY":
			firstCfg.noProxy = cmdLine[1]
			secondCfg.noProxy = cmdLine[1]
		}
	}
	cfgs = append(cfgs, firstCfg, secondCfg)
	return cfgs
}

// defaultLogger is a zerolog logr implementation.
func defaultLogger(level string) logr.Logger {
	zl := zerolog.New(os.Stdout)
	zl = zl.With().Caller().Timestamp().Logger()
	var l zerolog.Level
	switch level {
	case "debug":
		l = zerolog.DebugLevel
	default:
		l = zerolog.InfoLevel
	}
	zl = zl.Level(l)

	return zerologr.New(&zl)
}
