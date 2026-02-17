package probe

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"time"
)

// IsHTTP checks if the service on the given host:port speaks HTTP
// Sends a simple GET request and checks for HTTP response
func IsHTTP(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Try to connect with timeout
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Set read/write timeout
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	// Send a simple HTTP request
	request := "GET / HTTP/1.0\r\n\r\n"
	_, err = conn.Write([]byte(request))
	if err != nil {
		return false
	}

	// Read response
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	// Check if response starts with "HTTP/"
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "HTTP/")
}

// ProbeResult contains detailed information about an HTTP probe
type ProbeResult struct {
	IsHTTP   bool
	Response string
}

// Probe performs a detailed HTTP probe and returns the response status line
func Probe(host string, port int) ProbeResult {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return ProbeResult{IsHTTP: false}
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	request := "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n"
	_, err = conn.Write([]byte(request))
	if err != nil {
		return ProbeResult{IsHTTP: false}
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return ProbeResult{IsHTTP: false}
	}

	response := strings.TrimSpace(line)
	isHTTP := strings.HasPrefix(strings.ToUpper(response), "HTTP/")

	return ProbeResult{
		IsHTTP:   isHTTP,
		Response: response,
	}
}
