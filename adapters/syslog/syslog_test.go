package syslog

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
	"bytes"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"

	_ "github.com/gliderlabs/logspout/transports/tcp"
	_ "github.com/gliderlabs/logspout/transports/tls"
	_ "github.com/gliderlabs/logspout/transports/udp"
)

const (
	testPriority  = "{{.Priority}}"
	testTimestamp = "{{.Timestamp}}"
	testHostname  = "{{.Container.Config.Hostname}}"
	testTag       = "{{.ContainerName}}"
	testPid       = "{{.Container.State.Pid}}"
	testData      = "{{.Data}}"
	connCloseIdx  = 5
)

var (
	container = &docker.Container{
		ID:   "8dfafdbc3a40",
		Name: "\x00container",
		Config: &docker.Config{
			Hostname: "8dfafdbc3a40",
		},
	}
	testTmplStr = fmt.Sprintf("<%s>%s %s %s[%s]: %s\n",
		testPriority, testTimestamp, testHostname, testTag, testPid, testData)
	hostHostnameFilename = "/etc/host_hostname"
	hostnameContent      = "hostname"
	badHostnameContent   = "hostname\r\n"
)

func TestSyslogRetryCount(t *testing.T) {
	newRetryCount := uint(20)
	os.Setenv("RETRY_COUNT", strconv.Itoa(int(newRetryCount)))
	setRetryCount()
	if retryCount != newRetryCount {
		t.Errorf("expected %v got %v", newRetryCount, retryCount)
	}

	os.Unsetenv("RETRY_COUNT")
	setRetryCount()
	if retryCount != defaultRetryCount {
		t.Errorf("expected %v got %v", defaultRetryCount, retryCount)
	}
}

func TestSyslogReconnectOnClose(t *testing.T) {
	done := make(chan string)
	addr, sock, srvWG := startServer("tcp", "", done)
	defer srvWG.Wait()
	defer os.Remove(addr)
	defer sock.Close()
	route := &router.Route{Adapter: "syslog+tcp", Address: addr}
	adapter, err := NewSyslogAdapter(route)
	if err != nil {
		t.Fatal(err)
	}

	stream := make(chan *router.Message)
	go adapter.Stream(stream)

	count := 100
	messages := make(chan string, count)
	go sendLogstream(stream, messages, adapter, count)

	timeout := time.After(6 * time.Second)
	msgnum := 1
	for {
		select {
		case msg := <-done:
			// Don't check a message that we know was dropped
			if msgnum%connCloseIdx == 0 {
				_ = <-messages
				msgnum++
			}
			check(t, adapter.(*Adapter).tmpl, <-messages, msg)
			msgnum++
		case <-timeout:
			adapter.(*Adapter).conn.Close()
			t.Fatal("timeout after", msgnum, "messages")
			return
		default:
			if msgnum == count {
				adapter.(*Adapter).conn.Close()
				return
			}
		}
	}
}

func TestSyslogReplaceFunc(t *testing.T) {
	in := "{{ replace \"oink oink oink\" \"k\" \"ky\" 2}}"
	os.Setenv("SYSLOG_STRUCTURED_DATA", in)
	adapter, err := newDummyAdapter()
	if err != nil {
		t.Fatal(err)
	}

	out := new(bytes.Buffer)
	err = adapter.(*Adapter).tmpl.Execute(out, "")

	if err != nil {
		log.Fatalf("template error: %s\n", err)
	}

	expected := "<PRIORITY>1 TIMESTAMP HOSTNAME TAG PID - [oinky oinky oink] DATA\n"
	check(t, adapter.(*Adapter).tmpl, expected, out.String())
}

func TestSyslogJoinFunc(t *testing.T) {
	array := []string{"foo", "bar"}
	in := "{{ join . \"-\" }}"
	os.Setenv("SYSLOG_STRUCTURED_DATA", in)
	adapter, err := newDummyAdapter()
	if err != nil {
		t.Fatal(err)
	}

	out := new(bytes.Buffer)
	err = adapter.(*Adapter).tmpl.Execute(out, array)

	if err != nil {
		log.Fatalf("template error: %s\n", err)
	}

	expected :=  "<PRIORITY>1 TIMESTAMP HOSTNAME TAG PID - [foo-bar] DATA\n"
	check(t, adapter.(*Adapter).tmpl, expected, out.String())
}

func TestSyslogSplitFunc(t *testing.T) {
	in := "{{ index (split \"foo/bar\" \"/\") 1 }}"
	os.Setenv("SYSLOG_STRUCTURED_DATA", in)
	adapter, err := newDummyAdapter()
	if err != nil {
		t.Fatal(err)
	}

	out := new(bytes.Buffer)
	err = adapter.(*Adapter).tmpl.Execute(out, "")

	if err != nil {
		log.Fatalf("template error: %s\n", err)
	}

	expected := "<PRIORITY>1 TIMESTAMP HOSTNAME TAG PID - [bar] DATA\n"
	check(t, adapter.(*Adapter).tmpl, expected, out.String())
}

func TestHostnameDoesNotHaveLineFeed(t *testing.T) {
	if err := ioutil.WriteFile(hostHostnameFilename, []byte(badHostnameContent), 0777); err != nil {
		t.Fatal(err)
	}
	testHostname := getHostname()
	if strings.Contains(testHostname, badHostnameContent) {
		t.Errorf("expected hostname to be %s. got %s in hostname %s", hostnameContent, badHostnameContent, testHostname)
	}
}

func newDummyAdapter()(router.LogAdapter, error) {
	os.Setenv("SYSLOG_PRIORITY", "PRIORITY")
	os.Setenv("SYSLOG_TIMESTAMP", "TIMESTAMP")
	os.Setenv("SYSLOG_PID", "PID")
	os.Setenv("SYSLOG_HOSTNAME", "HOSTNAME")
	os.Setenv("SYSLOG_TAG", "TAG")
	os.Setenv("SYSLOG_DATA", "DATA")
	done := make(chan string)
	addr, sock, srvWG := startServer("tcp", "", done)
	defer srvWG.Wait()
	defer os.Remove(addr)
	defer sock.Close()
	route := &router.Route{Adapter: "syslog+tcp", Address: addr}
	return NewSyslogAdapter(route)
}

func startServer(n, la string, done chan<- string) (addr string, sock io.Closer, wg *sync.WaitGroup) {
	if n == "udp" || n == "tcp" {
		la = "127.0.0.1:0"
	}
	wg = new(sync.WaitGroup)

	l, err := net.Listen(n, la)
	if err != nil {
		log.Fatalf("startServer failed: %v", err)
	}
	addr = l.Addr().String()
	sock = l
	wg.Add(1)
	go func() {
		defer wg.Done()
		runStreamSyslog(l, done, wg)
	}()

	return
}

func runStreamSyslog(l net.Listener, done chan<- string, wg *sync.WaitGroup) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b := bufio.NewReader(c)
			var i = 1
			for {
				i++
				s, err := b.ReadString('\n')
				if err != nil {
					break
				}
				done <- s
				if i%connCloseIdx == 0 {
					break
				}
			}
			c.Close()
		}(c)
	}
}

func sendLogstream(stream chan *router.Message, messages chan string, adapter router.LogAdapter, count int) {
	for i := 1; i <= count; i++ {
		msg := &Message{
			Message: &router.Message{
				Container: container,
				Data:      "test " + strconv.Itoa(i),
				Time:      time.Now(),
				Source:    "stdout",
			},
		}
		stream <- msg.Message
		b, _ := msg.Render(adapter.(*Adapter).tmpl)
		messages <- string(b)
		time.Sleep(10 * time.Millisecond)
	}
}

func check(t *testing.T, tmpl *template.Template, in string, out string) {
	if in != out {
		t.Errorf("expected: %s\ngot: %s\n", in, out)
	}
}
