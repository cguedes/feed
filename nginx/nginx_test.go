package nginx

import (
	"testing"

	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"

	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/sky-uk/feed/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

const (
	port          = 9090
	fakeNginx     = "./fake_nginx.sh"
	smallWaitTime = time.Millisecond * 10
)

type mockSignaller struct {
	mock.Mock
}

func (m *mockSignaller) sigquit(p *os.Process) error {
	m.Called(p)
	return nil
}

func (m *mockSignaller) sighup(p *os.Process) error {
	m.Called(p)
	return nil
}

func newConf(tmpDir string, binary string) Conf {
	return Conf{
		WorkingDir:              tmpDir,
		BinaryLocation:          binary,
		IngressPort:             port,
		WorkerProcesses:         1,
		BackendKeepalives:       1024,
		BackendKeepaliveSeconds: 58,
	}
}

func newLb(tmpDir string) (controller.Updater, *mockSignaller) {
	return newLbWithBinary(tmpDir, fakeNginx)
}

func newLbWithBinary(tmpDir string, binary string) (controller.Updater, *mockSignaller) {
	conf := newConf(tmpDir, binary)
	return newLbWithConf(conf)
}

func newLbWithConf(conf Conf) (controller.Updater, *mockSignaller) {
	lb := New(conf)
	signaller := &mockSignaller{}
	signaller.On("sigquit", mock.AnythingOfType("*os.Process")).Return(nil)
	lb.(*nginxLoadBalancer).signaller = signaller
	return lb, signaller
}

func TestCanStartThenStop(t *testing.T) {
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb, mockSignaller := newLb(tmpDir)

	assert.NoError(t, lb.Start())
	assert.NoError(t, lb.Stop())
	mockSignaller.AssertExpectations(t)
}

func TestStopWaitsForGracefulShutdownOfNginx(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb, _ := newLbWithBinary(tmpDir, "./fake_graceful_nginx.py")
	lb.(*nginxLoadBalancer).signaller = &osSignaller{}

	assert.NoError(lb.Start())
	assert.NoError(lb.Stop())
	assert.Error(lb.Health(), "should have waited for nginx to gracefully stop")
}

func TestHealthyWhileRunning(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	ts := stubHealthPort()
	defer ts.Close()
	conf := newConf(tmpDir, fakeNginx)
	conf.HealthPort = getPort(ts)
	lb, _ := newLbWithConf(conf)

	assert.Error(lb.Health(), "should be unhealthy")
	assert.NoError(lb.Start())

	time.Sleep(smallWaitTime)
	assert.NoError(lb.Health(), "should be healthy")

	assert.NoError(lb.Stop())
	assert.Error(lb.Health(), "should be unhealthy")
}

func TestUnhealthyIfHealthPortIsNotUp(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb, _ := newLb(tmpDir)

	assert.NoError(lb.Start())

	time.Sleep(smallWaitTime)
	assert.Error(lb.Health(), "should be unhealthy")
}

func TestFailsIfNginxDiesEarly(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb, _ := newLbWithBinary(tmpDir, "./fake_failing_nginx.sh")

	assert.Error(lb.Start())
	assert.Error(lb.Health())
}

func TestCanSetLogLevel(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	defaultLogLevel := newConf(tmpDir, fakeNginx)
	customLogLevel := newConf(tmpDir, fakeNginx)
	customLogLevel.LogLevel = "info"

	var tests = []struct {
		nginxConf Conf
		logLine   string
	}{
		{
			defaultLogLevel,
			"error_log stderr warn;",
		},
		{
			customLogLevel,
			"error_log stderr info;",
		},
	}

	for _, test := range tests {
		lb, _ := newLbWithConf(test.nginxConf)
		assert.NoError(lb.Start())

		confBytes, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
		assert.NoError(err)
		conf := string(confBytes)

		assert.Contains(conf, test.logLine)
	}
}

func TestTrustedFrontendsSetsUpClientIPCorrectly(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	multipleTrusted := newConf(tmpDir, fakeNginx)
	multipleTrusted.TrustedFrontends = []string{"10.50.185.0/24", "10.82.0.0/16"}

	var tests = []struct {
		name           string
		lbConf         Conf
		expectedOutput string
	}{
		{
			"multiple trusted frontends works",
			multipleTrusted,
			`    # Obtain client IP from frontend's X-Forward-For header
    set_real_ip_from 10.50.185.0/24;
    set_real_ip_from 10.82.0.0/16;

    real_ip_header X-Forwarded-For;
    real_ip_recursive on;`,
		},
		{
			"no trusted frontends works",
			newConf(tmpDir, fakeNginx),
			`    # Obtain client IP from frontend's X-Forward-For header

    real_ip_header X-Forwarded-For;
    real_ip_recursive on;`,
		},
	}

	for _, test := range tests {
		lb, _ := newLbWithConf(test.lbConf)

		assert.NoError(lb.Start())
		err := lb.Update(controller.IngressUpdate{})
		assert.NoError(err)

		config, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
		assert.NoError(err)
		configContents := string(config)

		assert.Contains(configContents, test.expectedOutput, test.name)

	}
}

func TestNginxConfigUpdates(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	defaultConf := newConf(tmpDir, fakeNginx)

	var tests = []struct {
		name          string
		lbConf        Conf
		entries       []controller.IngressEntry
		configEntries []string
	}{
		{
			"Check full ingress entry works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
			},
			[]string{
				"   # chris-ingress\n" +
					"    upstream upstream000 {\n" +
					"        server service:9090;\n" +
					"        keepalive 1024;\n" +
					"    }\n" +
					"\n" +
					"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris.com;\n" +
					"\n" +
					"        # Restrict clients\n" +
					"        allow 127.0.0.1;\n" +
					"        allow 10.82.0.0/16;\n" +
					"        \n" +
					"        deny all;\n" +
					"\n" +
					"        location /path/ {\n" +
					"            # Strip location path when proxying.\n" +
					"            proxy_pass http://upstream000/;\n" +
					"\n" +
					"            # Enable keepalive to backend.\n" +
					"            proxy_http_version 1.1;\n" +
					"            proxy_set_header Connection \"\";\n" +
					"\n" +
					"            # Add X-Forwarded-For and X-Original-URI for proxy information.\n" +
					"            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n" +
					"            proxy_set_header X-Original-URI $request_uri;\n" +
					"\n            # Timeout faster than the default 60s on initial connect.\n" +
					"            proxy_connect_timeout 10s;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 58s;\n" +
					"            proxy_send_timeout 58s;\n" +
					"\n" +
					"            # Disable buffering, as we'll be interacting with ELBs with http listeners, which we assume will\n" +
					"            # quickly consume and generate responses and requests.\n" +
					"            # This should be enabled if nginx will directly serve traffic externally to unknown clients.\n" +
					"            proxy_buffering off;\n" +
					"            proxy_request_buffering off;\n" +
					"        }\n" +
					"    }\n" +
					"    ",
			},
		},
		{
			"Check no allows works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{},
				},
			},
			[]string{
				"   # chris-ingress\n" +
					"    upstream upstream000 {\n" +
					"        server service:9090;\n" +
					"        keepalive 1024;\n" +
					"    }\n" +
					"\n" +
					"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris.com;\n" +
					"\n" +
					"        # Restrict clients\n" +
					"        allow 127.0.0.1;\n" +
					"        \n" +
					"        deny all;\n",
			},
		},
		{
			"Check nil allow works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          nil,
				},
			},
			[]string{
				"   # chris-ingress\n" +
					"    upstream upstream000 {\n" +
					"        server service:9090;\n" +
					"        keepalive 1024;\n" +
					"    }\n" +
					"\n" +
					"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris.com;\n" +
					"\n" +
					"        # Restrict clients\n" +
					"        allow 127.0.0.1;\n" +
					"        \n" +
					"        deny all;\n",
			},
		},
		{
			"Check entries ordered by name",
			defaultConf,
			[]controller.IngressEntry{
				{
					Name:           "2-last-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Name:           "0-first-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Name:           "1-next-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
			},
			[]string{
				"   # 0-first-ingress\n" +
					"    upstream upstream000 {\n",
				"   # 1-next-ingress\n" +
					"    upstream upstream001 {\n",
				"   # 2-last-ingress\n" +
					"    upstream upstream002 {\n",
			},
		},
		{
			"Check proxy_pass ordered correctly",
			defaultConf,
			[]controller.IngressEntry{
				{
					Name:           "2-last-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Name:           "0-first-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Name:           "1-next-ingress",
					Host:           "foo.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
			},
			[]string{
				"    proxy_pass http://upstream000/;\n",
				"    proxy_pass http://upstream001/;\n",
				"    proxy_pass http://upstream002/;\n",
			},
		},
		{
			"Check path slashes are added correctly",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "/prefix-with-slash/",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "prefix-without-preslash/",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "/prefix-without-postslash",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "prefix-without-anyslash",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16"},
				},
			},
			[]string{
				"        location / {\n",
				"        location /prefix-with-slash/ {\n",
				"        location /prefix-without-preslash/ {\n",
				"        location /prefix-without-postslash/ {\n",
				"        location /prefix-without-anyslash/ {\n",
			},
		},
		{
			"Check multiple allows work",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Name:           "chris-ingress",
					Path:           "",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16", "10.99.0.0/16"},
				},
			},
			[]string{
				"        # Restrict clients\n" +
					"        allow 127.0.0.1;\n" +
					"        allow 10.82.0.0/16;\n" +
					"        allow 10.99.0.0/16;\n" +
					"        \n" +
					"        deny all;\n",
			},
		},
	}

	for _, test := range tests {
		lb, mockSignaller := newLbWithConf(test.lbConf)
		mockSignaller.On("sighup", mock.AnythingOfType("*os.Process")).Return(nil)

		assert.NoError(lb.Start())
		entries := test.entries
		err := lb.Update(controller.IngressUpdate{Entries: entries})
		assert.NoError(err)

		config, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
		assert.NoError(err)
		configContents := string(config)

		r, err := regexp.Compile("(?s)# Start entry\\n (.*?)# End entry")
		assert.NoError(err)
		serverEntries := r.FindAllStringSubmatch(configContents, -1)

		assert.Equal(len(test.configEntries), len(serverEntries))
		for i := range test.configEntries {
			expected := test.configEntries[i]
			actual := serverEntries[i][1]
			assert.True(strings.Contains(actual, expected),
				"%s\nExpected:\n%s\nActual:\n%s\n", test.name, expected, actual)
		}

		assert.Nil(lb.Stop())
		mockSignaller.AssertExpectations(t)
	}
}

func TestDoesNotUpdateIfConfigurationHasNotChanged(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)
	lb, mockSignaller := newLb(tmpDir)
	mockSignaller.On("sighup", mock.AnythingOfType("*os.Process")).Return(nil).Once()

	assert.NoError(lb.Start())

	entries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "service",
			ServicePort:    9090,
		},
	}

	assert.NoError(lb.Update(controller.IngressUpdate{Entries: entries}))
	config1, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
	assert.NoError(err)

	assert.NoError(lb.Update(controller.IngressUpdate{Entries: entries}))
	config2, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
	assert.NoError(err)

	assert.NoError(lb.Stop())

	assert.Equal(string(config1), string(config2), "configs should be identical")
	mockSignaller.AssertExpectations(t)
}

func TestUpdatesMetricsFromNginxStatusPage(t *testing.T) {
	// given
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	ts := stubHealthPort()
	defer ts.Close()

	conf := newConf(tmpDir, fakeNginx)
	conf.HealthPort = getPort(ts)
	lb, _ := newLbWithConf(conf)

	// when
	assert.NoError(lb.Start())
	time.Sleep(time.Millisecond * 50)

	// then
	assert.Equal(9.0, gaugeValue(connectionGauge))
	assert.Equal(2.0, gaugeValue(readingConnectionsGauge))
	assert.Equal(1.0, gaugeValue(writingConnectionsGauge))
	assert.Equal(8.0, gaugeValue(waitingConnectionsGauge))
	assert.Equal(13287.0, gaugeValue(acceptsGauge))
	assert.Equal(13286.0, gaugeValue(handledGauge))
	assert.Equal(66627.0, gaugeValue(requestsGauge))
}

func stubHealthPort() *httptest.Server {
	statusBody := `Active connections: 9
server accepts handled requests
 13287 13286 66627
Reading: 2 Writing: 1 Waiting: 8
`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			fmt.Fprintln(w, statusBody)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func getPort(ts *httptest.Server) int {
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	intPort, err := strconv.Atoi(port)
	if err != nil {
		panic(err)
	}
	return intPort
}

func gaugeValue(g prometheus.Gauge) float64 {
	metricCh := make(chan prometheus.Metric, 1)
	g.Collect(metricCh)
	metric := <-metricCh
	var metricVal dto.Metric
	metric.Write(&metricVal)
	return *metricVal.Gauge.Value
}

func TestFailsToUpdateIfConfigurationIsBroken(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)
	lb, mockSignaller := newLbWithBinary(tmpDir, "./fake_nginx_failing_reload.sh")
	mockSignaller.On("sighup", mock.AnythingOfType("*os.Process")).Return(nil).Once()

	assert.NoError(lb.Start())

	entries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "service",
			ServicePort:    9090,
		},
	}

	err := lb.Update(controller.IngressUpdate{Entries: entries})
	assert.Contains(err.Error(), "Config check failed")
	assert.Contains(err.Error(), "./fake_nginx_failing_reload.sh -t")
}

func setupWorkDir(t *testing.T) string {
	tmpDir, err := ioutil.TempDir(os.TempDir(), "ingress_lb_test")
	assert.NoError(t, err)
	copyNginxTemplate(t, tmpDir)
	return tmpDir
}

func copyNginxTemplate(t *testing.T, tmpDir string) {
	assert.NoError(t, exec.Command("cp", "nginx.tmpl", tmpDir+"/").Run())
}
