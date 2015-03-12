package main_test

import (
	"github.com/cloudfoundry-incubator/candiedyaml"
	"github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/route"
	"github.com/cloudfoundry/gorouter/test"
	"github.com/cloudfoundry/gorouter/test_util"
	"github.com/cloudfoundry/gunk/natsrunner"
	"github.com/cloudfoundry/yagnats"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
	. "github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/localip"

	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var _ = Describe("Router Integration", func() {
	var tmpdir string

	var natsPort uint16
	var natsRunner *natsrunner.NATSRunner

	var gorouterSession *Session

	writeConfig := func(config *config.Config, cfgFile string) {
		cfgBytes, err := candiedyaml.Marshal(config)
		Ω(err).ShouldNot(HaveOccurred())
		ioutil.WriteFile(cfgFile, cfgBytes, os.ModePerm)
	}

	createConfig := func(cfgFile string, statusPort, proxyPort uint16) *config.Config {
		config := test_util.SpecConfig(natsPort, statusPort, proxyPort)

		// ensure the threshold is longer than the interval that we check,
		// because we set the route's timestamp to time.Now() on the interval
		// as part of pausing
		config.PruneStaleDropletsIntervalInSeconds = 1
		config.DropletStaleThresholdInSeconds = 2
		config.StartResponseDelayIntervalInSeconds = 1
		config.EndpointTimeoutInSeconds = 5
		config.DrainTimeoutInSeconds = 1

		writeConfig(config, cfgFile)
		return config
	}

	startGorouterSession := func(cfgFile string) *Session {
		gorouterCmd := exec.Command(gorouterPath, "-c", cfgFile)
		session, err := Start(gorouterCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())
		Eventually(session, 5).Should(Say("gorouter.started"))
		gorouterSession = session

		return session
	}

	stopGorouter := func(gorouterSession *Session) {
		err := gorouterSession.Command.Process.Signal(syscall.SIGTERM)
		Ω(err).ShouldNot(HaveOccurred())
		Expect(gorouterSession.Wait(5 * time.Second)).Should(Exit(0))
	}

	BeforeEach(func() {
		var err error
		tmpdir, err = ioutil.TempDir("", "gorouter")
		Ω(err).ShouldNot(HaveOccurred())

		natsPort = test_util.NextAvailPort()
		natsRunner = natsrunner.NewNATSRunner(int(natsPort))
		natsRunner.Start()
	})

	AfterEach(func() {
		if natsRunner != nil {
			natsRunner.Stop()
		}

		os.RemoveAll(tmpdir)

		if gorouterSession != nil && gorouterSession.ExitCode() == -1 {
			stopGorouter(gorouterSession)
		}
	})

	Context("Drain", func() {
		var config *config.Config
		var localIP string
		var statusPort uint16
		var proxyPort uint16

		BeforeEach(func() {
			var err error
			localIP, err = localip.LocalIP()
			Ω(err).ShouldNot(HaveOccurred())

			statusPort = test_util.NextAvailPort()
			proxyPort = test_util.NextAvailPort()

			cfgFile := filepath.Join(tmpdir, "config.yml")
			config = createConfig(cfgFile, statusPort, proxyPort)

			gorouterSession = startGorouterSession(cfgFile)
		})

		It("waits for all requests to finish", func() {
			mbusClient, err := newMessageBus(config)
			Ω(err).ShouldNot(HaveOccurred())

			requestMade := make(chan bool)
			requestProcessing := make(chan bool)
			responseRead := make(chan bool)

			longApp := test.NewTestApp([]route.Uri{"longapp.vcap.me"}, proxyPort, mbusClient, nil)
			longApp.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
				requestMade <- true
				<-requestProcessing
				_, err := ioutil.ReadAll(r.Body)
				defer r.Body.Close()
				Ω(err).ShouldNot(HaveOccurred())
				w.WriteHeader(http.StatusOK)
				w.Write([]byte{'b'})
			})
			longApp.Listen()
			routesUri := fmt.Sprintf("http://%s:%s@%s:%d/routes", config.Status.User, config.Status.Pass, localIP, statusPort)
			Ω(waitAppRegistered(routesUri, longApp, 2*time.Second)).To(BeTrue())

			go func() {
				defer GinkgoRecover()
				resp, err := http.Get(longApp.Endpoint())
				Ω(err).ShouldNot(HaveOccurred())
				Ω(resp.StatusCode).Should(Equal(http.StatusOK))
				bytes, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				Ω(err).ShouldNot(HaveOccurred())
				Ω(bytes).Should(Equal([]byte{'b'}))
				responseRead <- true
			}()

			grouter := gorouterSession
			gorouterSession = nil

			<-requestMade

			err = grouter.Command.Process.Signal(syscall.SIGUSR1)

			requestProcessing <- true

			Ω(err).ShouldNot(HaveOccurred())
			Eventually(grouter, 5).Should(Exit(0))
			Eventually(responseRead).Should(Receive(BeTrue()))
		})

		It("will timeout if requests take too long", func() {
			mbusClient, err := newMessageBus(config)
			Ω(err).ShouldNot(HaveOccurred())

			blocker := make(chan bool)
			resultCh := make(chan error, 1)
			timeoutApp := test.NewTestApp([]route.Uri{"timeout.vcap.me"}, proxyPort, mbusClient, nil)
			timeoutApp.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
				blocker <- true
				<-blocker
			})
			timeoutApp.Listen()
			routesUri := fmt.Sprintf("http://%s:%s@%s:%d/routes", config.Status.User, config.Status.Pass, localIP, statusPort)
			Ω(waitAppRegistered(routesUri, timeoutApp, 2*time.Second)).To(BeTrue())

			go func() {
				_, err := http.Get(timeoutApp.Endpoint())
				resultCh <- err
			}()

			<-blocker
			defer func() {
				blocker <- true
			}()

			grouter := gorouterSession
			gorouterSession = nil
			err = grouter.Command.Process.Signal(syscall.SIGUSR1)
			Ω(err).ShouldNot(HaveOccurred())
			Eventually(grouter, 5).Should(Exit(0))

			var result error
			Eventually(resultCh, 5).Should(Receive(&result))
			Ω(result).Should(BeAssignableToTypeOf(&url.Error{}))
			urlErr := result.(*url.Error)
			Ω(urlErr.Err).Should(Equal(io.EOF))
		})

		It("prevents new connections", func() {
			mbusClient, err := newMessageBus(config)

			blocker := make(chan bool)
			timeoutApp := test.NewTestApp([]route.Uri{"timeout.vcap.me"}, proxyPort, mbusClient, nil)
			timeoutApp.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
				blocker <- true
				<-blocker
			})
			timeoutApp.Listen()
			routesUri := fmt.Sprintf("http://%s:%s@%s:%d/routes", config.Status.User, config.Status.Pass, localIP, statusPort)
			Ω(waitAppRegistered(routesUri, timeoutApp, 2*time.Second)).To(BeTrue())

			go func() {
				http.Get(timeoutApp.Endpoint())
			}()

			<-blocker
			defer func() {
				blocker <- true
			}()

			grouter := gorouterSession
			gorouterSession = nil
			err = grouter.Command.Process.Signal(syscall.SIGUSR1)
			Ω(err).ShouldNot(HaveOccurred())
			Eventually(grouter, 5).Should(Exit(0))

			_, err = http.Get(timeoutApp.Endpoint())
			Ω(err).Should(HaveOccurred())
			urlErr := err.(*url.Error)
			opErr := urlErr.Err.(*net.OpError)
			Ω(opErr.Op).Should(Equal("dial"))
		})
	})

	Context("When Dropsonde is misconfigured", func() {
		It("fails to start", func() {
			statusPort := test_util.NextAvailPort()
			proxyPort := test_util.NextAvailPort()

			cfgFile := filepath.Join(tmpdir, "config.yml")
			config := createConfig(cfgFile, statusPort, proxyPort)
			config.Logging.MetronAddress = ""
			writeConfig(config, cfgFile)

			gorouterCmd := exec.Command(gorouterPath, "-c", cfgFile)
			gorouterSession, _ = Start(gorouterCmd, GinkgoWriter, GinkgoWriter)
			Eventually(gorouterSession, 5).Should(Exit(1))
		})
	})

	It("has Nats connectivity", func() {
		localIP, err := localip.LocalIP()
		Ω(err).ShouldNot(HaveOccurred())

		statusPort := test_util.NextAvailPort()
		proxyPort := test_util.NextAvailPort()

		cfgFile := filepath.Join(tmpdir, "config.yml")
		config := createConfig(cfgFile, statusPort, proxyPort)

		gorouterSession = startGorouterSession(cfgFile)

		mbusClient, err := newMessageBus(config)

		zombieApp := test.NewGreetApp([]route.Uri{"zombie.vcap.me"}, proxyPort, mbusClient, nil)
		zombieApp.Listen()

		runningApp := test.NewGreetApp([]route.Uri{"innocent.bystander.vcap.me"}, proxyPort, mbusClient, nil)
		runningApp.Listen()

		routesUri := fmt.Sprintf("http://%s:%s@%s:%d/routes", config.Status.User, config.Status.Pass, localIP, statusPort)

		Ω(waitAppRegistered(routesUri, zombieApp, 2*time.Second)).To(BeTrue())
		Ω(waitAppRegistered(routesUri, runningApp, 2*time.Second)).To(BeTrue())

		heartbeatInterval := 200 * time.Millisecond
		zombieTicker := time.NewTicker(heartbeatInterval)
		runningTicker := time.NewTicker(heartbeatInterval)

		go func() {
			for {
				select {
				case <-zombieTicker.C:
					zombieApp.Register()
				case <-runningTicker.C:
					runningApp.Register()
				}
			}
		}()

		zombieApp.VerifyAppStatus(200)

		// Give enough time to register multiple times
		time.Sleep(heartbeatInterval * 3)

		// kill registration ticker => kill app (must be before stopping NATS since app.Register is fake and queues messages in memory)
		zombieTicker.Stop()

		natsRunner.Stop()

		staleCheckInterval := config.PruneStaleDropletsInterval
		staleThreshold := config.DropletStaleThreshold
		// Give router time to make a bad decision (i.e. prune routes)
		time.Sleep(staleCheckInterval + staleThreshold + 250*time.Millisecond)

		// While NATS is down no routes should go down
		zombieApp.VerifyAppStatus(200)
		runningApp.VerifyAppStatus(200)

		natsRunner.Start()

		// Right after NATS starts up all routes should stay up
		zombieApp.VerifyAppStatus(200)
		runningApp.VerifyAppStatus(200)

		zombieGone := make(chan bool)

		go func() {
			for {
				// Finally the zombie is cleaned up. Maybe proactively enqueue Unregister events in DEA's.
				err := zombieApp.CheckAppStatus(404)
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				err = runningApp.CheckAppStatus(200)
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				zombieGone <- true

				break
			}
		}()

		waitTime := staleCheckInterval + staleThreshold + 5*time.Second
		Eventually(zombieGone, waitTime.Seconds()).Should(Receive())
	})

	It("has references to the routing api", func() {
		server1 := ghttp.NewServer()
		server1.AppendHandlers(ghttp.VerifyRequest("GET", "/v1/routes"))
		server1Port, _ := strconv.Atoi(strings.Split(server1.Addr(), ":")[1])
		defer server1.Close()

		server2 := ghttp.NewServer()
		server2.AppendHandlers(ghttp.VerifyRequest("GET", "/v1/routes"))
		server2Port, _ := strconv.Atoi(strings.Split(server2.Addr(), ":")[1])
		defer server2.Close()

		cfgFile := filepath.Join(tmpdir, "config.yml")
		statusPort := test_util.NextAvailPort()
		proxyPort := test_util.NextAvailPort()
		c := createConfig(cfgFile, statusPort, proxyPort)
		c.RoutingAPI.Route = "routing-api.127.0.0.1.xip.io"
		c.RoutingAPI.Hosts = []config.HostConfig{
			{
				Address: "127.0.0.1",
				Port:    uint16(server1Port),
			},
			{
				Address: "127.0.0.1",
				Port:    uint16(server2Port),
			},
		}
		writeConfig(c, cfgFile)

		gorouterSession = startGorouterSession(cfgFile)
		Eventually(func() int {
			return len(server1.ReceivedRequests()) + len(server2.ReceivedRequests())
		}).Should(Equal(1))
	})

	It("polls the routing api for new routes", func() {
		server := ghttp.NewServer()
		server.AppendHandlers(ghttp.VerifyRequest("GET", "/"))
		serverPort, _ := strconv.Atoi(strings.Split(server.Addr(), ":")[1])
		defer server.Close()

		routingAPI := ghttp.NewServer()
		routingAPI.AppendHandlers(
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/v1/routes"),
				ghttp.RespondWith(http.StatusOK, `[
				{"route":"da-route.127.0.0.1.xip.io","ip":"127.0.0.1", "port": `+strconv.Itoa(serverPort)+`}
				]`),
			),
		)
		routingAPIPort, _ := strconv.Atoi(strings.Split(routingAPI.Addr(), ":")[1])
		defer routingAPI.Close()

		cfgFile := filepath.Join(tmpdir, "config.yml")
		statusPort := test_util.NextAvailPort()
		proxyPort := test_util.NextAvailPort()
		c := createConfig(cfgFile, statusPort, proxyPort)
		c.RoutingAPI.Route = "routing-api.127.0.0.1.xip.io"
		c.RoutingAPI.Hosts = []config.HostConfig{
			{
				Address: "127.0.0.1",
				Port:    uint16(routingAPIPort),
			},
		}
		writeConfig(c, cfgFile)

		gorouterSession = startGorouterSession(cfgFile)
		Eventually(func() error {
			_, err := http.Get("http://da-route.127.0.0.1.xip.io:" + strconv.Itoa(int(proxyPort)))
			return err
		}).ShouldNot((HaveOccurred()))
		Eventually(server.ReceivedRequests).Should(HaveLen(1))
	})
})

func newMessageBus(c *config.Config) (yagnats.NATSConn, error) {
	natsMembers := make([]string, len(c.Nats))
	for _, info := range c.Nats {
		uri := url.URL{
			Scheme: "nats",
			User:   url.UserPassword(info.User, info.Pass),
			Host:   fmt.Sprintf("%s:%d", info.Host, info.Port),
		}
		natsMembers = append(natsMembers, uri.String())
	}

	return yagnats.Connect(natsMembers)
}

func waitAppRegistered(routesUri string, app *test.TestApp, timeout time.Duration) bool {
	return waitMsgReceived(routesUri, app, true, timeout)
}

func waitAppUnregistered(routesUri string, app *test.TestApp, timeout time.Duration) bool {
	return waitMsgReceived(routesUri, app, false, timeout)
}

func waitMsgReceived(uri string, app *test.TestApp, expectedToBeFound bool, timeout time.Duration) bool {
	interval := time.Millisecond * 50
	repetitions := int(timeout / interval)

	for j := 0; j < repetitions; j++ {
		resp, err := http.Get(uri)
		if err == nil {
			switch resp.StatusCode {
			case http.StatusOK:
				bytes, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				Ω(err).ShouldNot(HaveOccurred())
				routes := make(map[string][]string)
				err = json.Unmarshal(bytes, &routes)
				Ω(err).ShouldNot(HaveOccurred())
				route := routes[string(app.Urls()[0])]
				if expectedToBeFound {
					if route != nil {
						return true
					}
				} else {
					if route == nil {
						return true
					}
				}
			default:
				println("Failed to receive routes: ", resp.StatusCode, uri)
			}
		}

		time.Sleep(interval)
	}

	return false
}
