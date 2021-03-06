package nginx

import (
	"io/ioutil"
	"os"
	"os/exec"

	"bytes"
	"fmt"
	"text/template"

	"strings"

	"time"

	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/sky-uk/feed/controller"
	"github.com/sky-uk/feed/util"
)

const (
	nginxStartDelay       = time.Millisecond * 100
	metricsUpdateInterval = time.Second * 10
)

// Conf configuration for nginx
type Conf struct {
	BinaryLocation          string
	WorkingDir              string
	WorkerProcesses         int
	WorkerConnections       int
	KeepaliveSeconds        int
	BackendKeepalives       int
	BackendKeepaliveSeconds int
	HealthPort              int
	TrustedFrontends        []string
	IngressPort             int
	LogLevel                string
}

// Signaller interface around signalling the loadbalancer process
type signaller interface {
	sigquit(*os.Process) error
	sighup(*os.Process) error
}

type osSignaller struct {
}

// Sigquit sends a SIGQUIT to the process
func (s *osSignaller) sigquit(p *os.Process) error {
	log.Debugf("Sending SIGQUIT to %d", p.Pid)
	return p.Signal(syscall.SIGQUIT)
}

// Sighup sends a SIGHUP to the process
func (s *osSignaller) sighup(p *os.Process) error {
	log.Debugf("Sending SIGHUP to %d", p.Pid)
	return p.Signal(syscall.SIGHUP)
}

// Nginx implementation
type nginxLoadBalancer struct {
	Conf
	cmd              *exec.Cmd
	signaller        signaller
	running          util.SafeBool
	lastErr          util.SafeError
	metricsUnhealthy util.SafeBool
	doneCh           chan struct{}
}

// Used for generating nginx config
type loadBalancerTemplate struct {
	Conf
	Entries []nginxEntry
}

type nginxEntry struct {
	controller.IngressEntry
	UpstreamID string
}

func (lb *nginxLoadBalancer) nginxConfFile() string {
	return lb.WorkingDir + "/nginx.conf"
}

// New creates an nginx proxy.
func New(nginxConf Conf) controller.Updater {
	nginxConf.WorkingDir = strings.TrimSuffix(nginxConf.WorkingDir, "/")
	if nginxConf.LogLevel == "" {
		nginxConf.LogLevel = "warn"
	}

	return &nginxLoadBalancer{
		Conf:      nginxConf,
		signaller: &osSignaller{},
		doneCh:    make(chan struct{}),
	}
}

func (lb *nginxLoadBalancer) Start() error {
	if err := lb.logNginxVersion(); err != nil {
		return err
	}

	if err := lb.initialiseNginxConf(); err != nil {
		return fmt.Errorf("unable to initialise nginx config: %v", err)
	}

	lb.cmd = exec.Command(lb.BinaryLocation, "-c", lb.nginxConfFile())

	lb.cmd.Stdout = log.StandardLogger().Writer()
	lb.cmd.Stderr = log.StandardLogger().Writer()
	lb.cmd.Stdin = os.Stdin

	if err := lb.cmd.Start(); err != nil {
		return fmt.Errorf("unable to start nginx: %v", err)
	}

	lb.running.Set(true)
	go lb.waitForNginxToFinish()

	time.Sleep(nginxStartDelay)
	if !lb.running.Get() {
		return fmt.Errorf("nginx died shortly after starting")
	}

	go lb.periodicallyUpdateMetrics()

	log.Debugf("Nginx pid %d", lb.cmd.Process.Pid)
	return nil
}

func (lb *nginxLoadBalancer) logNginxVersion() error {
	cmd := exec.Command(lb.BinaryLocation, "-v")
	cmd.Stdout = log.StandardLogger().Writer()
	cmd.Stderr = log.StandardLogger().Writer()
	return cmd.Run()
}

func (lb *nginxLoadBalancer) initialiseNginxConf() error {
	err := os.Remove(lb.nginxConfFile())
	if err != nil {
		log.Debugf("Can't remove nginx.conf: %v", err)
	}
	_, err = lb.update(controller.IngressUpdate{Entries: []controller.IngressEntry{}})
	return err
}

func (lb *nginxLoadBalancer) waitForNginxToFinish() {
	err := lb.cmd.Wait()
	if err != nil {
		log.Error("Nginx has exited with an error: ", err)
	} else {
		log.Info("Nginx has shutdown successfully")
	}
	lb.running.Set(false)
	lb.lastErr.Set(err)
	close(lb.doneCh)
}

func (lb *nginxLoadBalancer) periodicallyUpdateMetrics() {
	lb.updateMetrics()
	ticker := time.NewTicker(metricsUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-lb.doneCh:
			return
		case <-ticker.C:
			lb.updateMetrics()
		}
	}
}

func (lb *nginxLoadBalancer) updateMetrics() {
	if err := parseAndSetNginxMetrics(lb.HealthPort, "/status"); err != nil {
		log.Warnf("Unable to update nginx metrics: %v", err)
		lb.metricsUnhealthy.Set(true)
	} else {
		lb.metricsUnhealthy.Set(false)
	}
}

func (lb *nginxLoadBalancer) Stop() error {
	log.Info("Shutting down nginx process")
	lb.cmd.Process.Signal(syscall.SIGQUIT)
	if err := lb.signaller.sigquit(lb.cmd.Process); err != nil {
		return fmt.Errorf("error shutting down nginx: %v", err)
	}
	<-lb.doneCh
	return lb.lastErr.Get()
}

func (lb *nginxLoadBalancer) Update(entries controller.IngressUpdate) error {
	updated, err := lb.update(entries)
	if err != nil {
		return fmt.Errorf("unable to update nginx: %v", err)
	}

	if updated {
		err = lb.signaller.sighup(lb.cmd.Process)
		if err != nil {
			return fmt.Errorf("unable to signal nginx to reload: %v", err)
		}
		log.Info("Nginx updated")
	}

	return nil
}

func (lb *nginxLoadBalancer) update(entries controller.IngressUpdate) (bool, error) {
	log.Debugf("Updating loadbalancer %s", entries)
	updatedConfig, err := lb.createConfig(entries)
	if err != nil {
		return false, err
	}

	existingConfig, err := ioutil.ReadFile(lb.nginxConfFile())
	if err != nil {
		log.Debugf("Error trying to read nginx.conf: %v", err)
		log.Info("Creating nginx.conf for the first time")
		return writeFile(lb.nginxConfFile(), updatedConfig)
	}

	return lb.diffAndUpdate(existingConfig, updatedConfig)
}

func (lb *nginxLoadBalancer) diffAndUpdate(existing, updated []byte) (bool, error) {
	diffOutput, err := diff(existing, updated)
	if err != nil {
		log.Warnf("Unable to diff nginx files: %v", err)
		return false, err
	}

	if len(diffOutput) == 0 {
		log.Info("Configuration has not changed")
		return false, nil
	}

	log.Infof("Updating nginx config: %s", string(diffOutput))
	_, err = writeFile(lb.nginxConfFile(), updated)

	if err != nil {
		log.Errorf("Unable to write nginx configuration: %v", err)
		return false, err
	}

	err = lb.checkNginxConfig()
	if err != nil {
		return false, err
	}

	return true, nil
}

func (lb *nginxLoadBalancer) checkNginxConfig() error {
	cmd := exec.Command(lb.BinaryLocation, "-t", "-c", lb.nginxConfFile())
	var out bytes.Buffer
	cmd.Stderr = &out
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("invalid config: %v: %s", err, out.String())
	}
	return nil
}

func (lb *nginxLoadBalancer) createConfig(update controller.IngressUpdate) ([]byte, error) {
	tmpl, err := template.New("nginx.tmpl").ParseFiles(lb.WorkingDir + "/nginx.tmpl")
	if err != nil {
		return nil, err
	}

	sortedIngressEntries := update.SortedByName().Entries

	var entries []nginxEntry
	for idx, ingressEntry := range sortedIngressEntries {
		trimmedPath := strings.TrimSuffix(strings.TrimPrefix(ingressEntry.Path, "/"), "/")
		if len(trimmedPath) == 0 {
			ingressEntry.Path = "/"
		} else {
			ingressEntry.Path = fmt.Sprintf("/%s/", trimmedPath)
		}

		entry := nginxEntry{
			IngressEntry: ingressEntry,
			UpstreamID:   fmt.Sprintf("upstream%03d", idx),
		}
		entries = append(entries, entry)
	}

	var output bytes.Buffer
	err = tmpl.Execute(&output, loadBalancerTemplate{Conf: lb.Conf, Entries: entries})

	if err != nil {
		return []byte{}, fmt.Errorf("Unable to execute nginx config duration. It will be out of date: %v", err)
	}

	return output.Bytes(), nil
}

func (lb *nginxLoadBalancer) Health() error {
	if !lb.running.Get() {
		return fmt.Errorf("nginx is not running")
	}
	if lb.metricsUnhealthy.Get() {
		return fmt.Errorf("nginx metrics are failing to update")
	}
	return nil
}

func (lb *nginxLoadBalancer) String() string {
	return "nginx proxy"
}

func writeFile(location string, contents []byte) (bool, error) {
	err := ioutil.WriteFile(location, contents, 0644)
	if err != nil {
		return false, err
	}
	return true, nil
}

func diff(b1, b2 []byte) ([]byte, error) {
	f1, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f1.Name())
	defer f1.Close()

	f2, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f2.Name())
	defer f2.Close()

	f1.Write(b1)
	f2.Write(b2)

	data, err := exec.Command("diff", "-u", f1.Name(), f2.Name()).CombinedOutput()
	if len(data) > 0 {
		return data, nil
	}
	return data, err
}
