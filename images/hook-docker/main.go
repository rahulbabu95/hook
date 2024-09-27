package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type tinkConfig struct {
	syslogHost         string
	insecureRegistries []string
	httpProxy          string
	httpsProxy         string
	noProxy            string
}

type dockerConfig struct {
	Debug              bool              `json:"debug"`
	LogDriver          string            `json:"log-driver,omitempty"`
	LogOpts            map[string]string `json:"log-opts,omitempty"`
	InsecureRegistries []string          `json:"insecure-registries,omitempty"`
}

func run() error {
	// Parse the cmdline in order to find the urls for the repository and path to the cert
	content, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	cmdLines := strings.Split(string(content), " ")
	cfgs := parseCmdLine(cmdLines)
	var runErr error
	// Generate the path to the tink-worker
	// loop through each configs to see if the tink server responds on any one
	for _, cfg := range cfgs {
		fmt.Println("Starting the Docker Engine")

		d := dockerConfig{
			Debug:     true,
			LogDriver: "syslog",
			LogOpts: map[string]string{
				"syslog-address": fmt.Sprintf("udp://%v:514", cfg.syslogHost),
			},
			InsecureRegistries: cfg.insecureRegistries,
		}
		path := "/etc/docker"
		// Create the directory for the docker config
		err = os.MkdirAll(path, os.ModeDir)
		if err != nil {
			return err
		}
		if err := d.writeToDisk(filepath.Join(path, "daemon.json")); err != nil {
			return fmt.Errorf("failed to write docker config: %w", err)
		}
		// Build the command, and execute
		// cmd := exec.Command("/usr/local/bin/docker-init", "/usr/local/bin/dockerd")
		cmd := exec.Command("sh", "-c", "/usr/local/bin/dockerd-entrypoint.sh")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		myEnvs := make([]string, 0, 3)
		myEnvs = append(myEnvs, fmt.Sprintf("HTTP_PROXY=%s", cfg.httpProxy))
		myEnvs = append(myEnvs, fmt.Sprintf("HTTPS_PROXY=%s", cfg.httpsProxy))
		myEnvs = append(myEnvs, fmt.Sprintf("NO_PROXY=%s", cfg.noProxy))

		cmd.Env = append(os.Environ(), myEnvs...)

		err = cmd.Run()
		if err != nil {
			runErr = err
			fmt.Printf("Error running docker using host %s: %v\n", cfg.syslogHost, err)
		}
	}
	// } else {
	// 	break
	// }
	// }

	return runErr
}

func main() {
	fmt.Println("Starting Docker")
	go rebootWatch()
	for {
		if err := run(); err != nil {
			fmt.Println("error starting up Docker", err)
			fmt.Println("will retry in 10 seconds")
			time.Sleep(10 * time.Second)
		}
	}
}

// writeToDisk writes the dockerConfig to loc.
func (d dockerConfig) writeToDisk(loc string) error {
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("unable to marshal docker config: %w", err)
	}
	if err := os.WriteFile(loc, b, 0o600); err != nil {
		return fmt.Errorf("error writing daemon.json: %w", err)
	}

	return nil
}

// parseCmdLine will parse the command line.
func parseCmdLine(cmdLines []string) (cfgs []tinkConfig) {
	firstCfg := tinkConfig{}
	secondCfg := tinkConfig{}
	for i := range cmdLines {
		cmdLine := strings.SplitN(cmdLines[i], "=", 2)
		if len(cmdLine) == 0 {
			continue
		}

		switch cmd := cmdLine[0]; cmd {
		// Provide Host IPs on command line as string slice and for POC
		// assume there will be only two separated by ','.
		case "syslog_host":
			hostIps := strings.Split(cmdLine[1], ",")
			firstCfg.syslogHost = hostIps[0]
			secondCfg.syslogHost = hostIps[1]
			// cfg.syslogHost = hostIps
		case "insecure_registries":
			firstCfg.insecureRegistries = strings.Split(cmdLine[1], ",")
			secondCfg.insecureRegistries = strings.Split(cmdLine[1], ",")
			// cfg.insecureRegistries = strings.Split(cmdLine[1], ",")
		case "HTTP_PROXY":
			firstCfg.httpProxy = cmdLine[1]
			secondCfg.httpProxy = cmdLine[1]
			// cfg.httpProxy = cmdLine[1]
		case "HTTPS_PROXY":
			firstCfg.httpsProxy = cmdLine[1]
			secondCfg.httpsProxy = cmdLine[1]
			// cfg.httpsProxy = cmdLine[1]
		case "NO_PROXY":
			firstCfg.noProxy = cmdLine[1]
			secondCfg.noProxy = cmdLine[1]
			// cfg.noProxy = cmdLine[1]
		}
	}
	cfgs = append(cfgs, firstCfg, secondCfg)
	return cfgs
}

func rebootWatch() {
	fmt.Println("Starting Reboot Watcher")

	// Forever loop
	for {
		if fileExists("/worker/reboot") {
			cmd := exec.Command("/sbin/reboot")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				fmt.Printf("error calling /sbin/reboot: %v\n", err)
				time.Sleep(time.Second)
				continue
			}
			break
		}
		// Wait one second before looking for file
		time.Sleep(time.Second)
	}
	fmt.Println("Rebooting")
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
