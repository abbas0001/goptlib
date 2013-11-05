// Tor pluggable transports library.
//
// Sample client usage:
//
// import "git.torproject.org/pluggable-transports/goptlib.git"
// var ptInfo pt.ClientInfo
// var err error
// ptInfo, err := pt.ClientSetup([]string{"foo"})
// if err != nil {
// 	os.Exit(1)
// }
// for _, methodName := range ptInfo.MethodNames {
// 	ln, err := startSocksListener()
// 	if err != nil {
// 		pt.CmethodError(methodName, err.Error())
// 		continue
// 	}
// 	pt.Cmethod(methodName, "socks4", ln.Addr())
// }
// pt.CmethodsDone()
//
// Sample server usage:
//
// import "git.torproject.org/pluggable-transports/goptlib.git"
// var ptInfo pt.ServerInfo
// var err error
// ptInfo, err = pt.ServerSetup([]string{"foo", "bar"})
// if err != nil {
// 	os.Exit(1)
// }
// for _, bindAddr := range ptInfo.BindAddrs {
// 	ln, err := startListener(bindAddr.Addr, bindAddr.MethodName)
// 	if err != nil {
// 		pt.SmethodError(bindAddr.MethodName, err.Error())
// 		continue
// 	}
// 	pt.Smethod(bindAddr.MethodName, ln.Addr())
// }
// pt.SmethodsDone()
// func handler(conn net.Conn, methodName string) {
// 	or, err := pt.ConnectOr(&ptInfo, conn, methodName)
// 	if err != nil {
// 		return
// 	}
// 	// Do something with or and conn.
// }

package pt

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Represents an error that can happen during negotiation, for example
// ENV-ERROR. When an error occurs, we print it to stdout and also pass it up
// the return chain.
type ptErr struct {
	Keyword string
	Args    []string
}

// Implements the error interface.
func (err *ptErr) Error() string {
	return formatLine(err.Keyword, err.Args...)
}

func getenv(key string) string {
	return os.Getenv(key)
}

// Returns an ENV-ERROR if the environment variable isn't set.
func getenvRequired(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", EnvError(fmt.Sprintf("no %s environment variable", key))
	}
	return value, nil
}

// Escape a string so it contains no byte values over 127 and doesn't contain
// any of the characters '\x00' or '\n'.
func escape(s string) string {
	var buf bytes.Buffer
	for _, b := range []byte(s) {
		if b == '\n' {
			buf.WriteString("\\n")
		} else if b == '\\' {
			buf.WriteString("\\\\")
		} else if 0 < b && b < 128 {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "\\x%02x", b)
		}
	}
	return buf.String()
}

func formatLine(keyword string, v ...string) string {
	var buf bytes.Buffer
	buf.WriteString(keyword)
	for _, x := range v {
		buf.WriteString(" " + escape(x))
	}
	return buf.String()
}

// Print a pluggable transports protocol line to stdout. The line consists of an
// unescaped keyword, followed by any number of escaped strings.
func Line(keyword string, v ...string) {
	fmt.Println(formatLine(keyword, v...))
	os.Stdout.Sync()
}

// Emit and return the given error as a ptErr.
func doError(keyword string, v ...string) *ptErr {
	Line(keyword, v...)
	return &ptErr{keyword, v}
}

// Emit an ENV-ERROR with explanation text. Return the error as a ptErr.
func EnvError(msg string) error {
	return doError("ENV-ERROR", msg)
}

// Emit a VERSION-ERROR with explanation text. Return the error as a ptErr.
func VersionError(msg string) error {
	return doError("VERSION-ERROR", msg)
}

// Emit a CMETHOD-ERROR with explanation text. Return the error as a ptErr.
func CmethodError(methodName, msg string) error {
	return doError("CMETHOD-ERROR", methodName, msg)
}

// Emit an SMETHOD-ERROR with explanation text. Return the error as a ptErr.
func SmethodError(methodName, msg string) error {
	return doError("SMETHOD-ERROR", methodName, msg)
}

// Emit a CMETHOD line. socks must be "socks4" or "socks5". Call this once for
// each listening client SOCKS port.
func Cmethod(name string, socks string, addr net.Addr) {
	Line("CMETHOD", name, socks, addr.String())
}

// Emit a CMETHODS DONE line. Call this after opening all client listeners.
func CmethodsDone() {
	Line("CMETHODS", "DONE")
}

// Emit an SMETHOD line. Call this once for each listening server port.
func Smethod(name string, addr net.Addr) {
	Line("SMETHOD", name, addr.String())
}

// Emit an SMETHODS DONE line. Call this after opening all server listeners.
func SmethodsDone() {
	Line("SMETHODS", "DONE")
}

// Get a pluggable transports version offered by Tor and understood by us, if
// any. The only version we understand is "1". This function reads the
// environment variable TOR_PT_MANAGED_TRANSPORT_VER.
func getManagedTransportVer() (string, error) {
	const transportVersion = "1"
	managedTransportVer, err := getenvRequired("TOR_PT_MANAGED_TRANSPORT_VER")
	if err != nil {
		return "", err
	}
	for _, offered := range strings.Split(managedTransportVer, ",") {
		if offered == transportVersion {
			return offered, nil
		}
	}
	return "", VersionError("no-version")
}

// Get the intersection of the method names offered by Tor and those in
// methodNames. This function reads the environment variable
// TOR_PT_CLIENT_TRANSPORTS.
func getClientTransports(methodNames []string) ([]string, error) {
	clientTransports, err := getenvRequired("TOR_PT_CLIENT_TRANSPORTS")
	if err != nil {
		return nil, err
	}
	if clientTransports == "*" {
		return methodNames, nil
	}
	result := make([]string, 0)
	for _, requested := range strings.Split(clientTransports, ",") {
		for _, methodName := range methodNames {
			if requested == methodName {
				result = append(result, methodName)
				break
			}
		}
	}
	return result, nil
}

// This structure is returned by ClientSetup. It consists of a list of method
// names.
type ClientInfo struct {
	MethodNames []string
}

// Check the client pluggable transports environments, emitting an error message
// and exiting the program if any error is encountered. Returns a subset of
// methodNames requested by Tor.
func ClientSetup(methodNames []string) (ClientInfo, error) {
	var info ClientInfo

	ver, err := getManagedTransportVer()
	if err != nil {
		return info, err
	}
	Line("VERSION", ver)

	info.MethodNames, err = getClientTransports(methodNames)
	if err != nil {
		return info, err
	}

	return info, nil
}

// A combination of a method name and an address, as extracted from
// TOR_PT_SERVER_BINDADDR.
type BindAddr struct {
	MethodName string
	Addr       *net.TCPAddr
}

// Resolve an address string into a net.TCPAddr.
func resolveBindAddr(bindAddr string) (*net.TCPAddr, error) {
	addr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err == nil {
		return addr, nil
	}
	// Before the fixing of bug #7011, tor doesn't put brackets around IPv6
	// addresses. Split after the last colon, assuming it is a port
	// separator, and try adding the brackets.
	parts := strings.Split(bindAddr, ":")
	if len(parts) <= 2 {
		return nil, err
	}
	bindAddr = "[" + strings.Join(parts[:len(parts)-1], ":") + "]:" + parts[len(parts)-1]
	return net.ResolveTCPAddr("tcp", bindAddr)
}

// Return a new slice, the members of which are those members of addrs having a
// MethodName in methodNames.
func filterBindAddrs(addrs []BindAddr, methodNames []string) []BindAddr {
	var result []BindAddr

	for _, ba := range addrs {
		for _, methodName := range methodNames {
			if ba.MethodName == methodName {
				result = append(result, ba)
				break
			}
		}
	}

	return result
}

// Return a map from method names to bind addresses. The map is the contents of
// TOR_PT_SERVER_BINDADDR, with keys filtered by TOR_PT_SERVER_TRANSPORTS, and
// further filtered by the methods in methodNames.
func getServerBindAddrs(methodNames []string) ([]BindAddr, error) {
	var result []BindAddr

	// Get the list of all requested bindaddrs.
	serverBindAddr, err := getenvRequired("TOR_PT_SERVER_BINDADDR")
	if err != nil {
		return nil, err
	}
	for _, spec := range strings.Split(serverBindAddr, ",") {
		var bindAddr BindAddr

		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			return nil, EnvError(fmt.Sprintf("TOR_PT_SERVER_BINDADDR: %q: doesn't contain \"-\"", spec))
		}
		bindAddr.MethodName = parts[0]
		addr, err := resolveBindAddr(parts[1])
		if err != nil {
			return nil, EnvError(fmt.Sprintf("TOR_PT_SERVER_BINDADDR: %q: %s", spec, err.Error()))
		}
		bindAddr.Addr = addr
		result = append(result, bindAddr)
	}

	// Filter by TOR_PT_SERVER_TRANSPORTS.
	serverTransports, err := getenvRequired("TOR_PT_SERVER_TRANSPORTS")
	if err != nil {
		return nil, err
	}
	if serverTransports != "*" {
		result = filterBindAddrs(result, strings.Split(serverTransports, ","))
	}

	// Finally filter by what we understand.
	result = filterBindAddrs(result, methodNames)

	return result, nil
}

// Read and validate the contents of an auth cookie file. Returns the 32-byte
// cookie. See section 4.2.1.2 of pt-spec.txt.
func readAuthCookieFile(filename string) ([]byte, error) {
	authCookieHeader := []byte("! Extended ORPort Auth Cookie !\x0a")
	header := make([]byte, 32)
	cookie := make([]byte, 32)

	f, err := os.Open(filename)
	if err != nil {
		return cookie, err
	}
	defer f.Close()

	n, err := io.ReadFull(f, header)
	if err != nil {
		return cookie, err
	}
	n, err = io.ReadFull(f, cookie)
	if err != nil {
		return cookie, err
	}
	// Check that the file ends here.
	n, err = f.Read(make([]byte, 1))
	if n != 0 {
		return cookie, errors.New(fmt.Sprintf("file is longer than 64 bytes"))
	} else if err != io.EOF {
		return cookie, errors.New(fmt.Sprintf("did not find EOF at end of file"))
	}

	if !bytes.Equal(header, authCookieHeader) {
		return cookie, errors.New(fmt.Sprintf("missing auth cookie header"))
	}

	return cookie, nil
}

// This structure is returned by ServerSetup. It consists of a list of
// BindAddrs, an address for the ORPort, an address for the extended ORPort (if
// any), and an authentication cookie (if any).
type ServerInfo struct {
	BindAddrs      []BindAddr
	OrAddr         *net.TCPAddr
	ExtendedOrAddr *net.TCPAddr
	AuthCookie     []byte
}

// Check the server pluggable transports environments, emitting an error message
// and exiting the program if any error is encountered. Resolves the various
// requested bind addresses, the server ORPort and extended ORPort, and reads
// the auth cookie file. Returns a ServerInfo struct.
func ServerSetup(methodNames []string) (ServerInfo, error) {
	var info ServerInfo

	ver, err := getManagedTransportVer()
	if err != nil {
		return info, err
	}
	Line("VERSION", ver)

	orPort, err := getenvRequired("TOR_PT_ORPORT")
	if err != nil {
		return info, err
	}
	info.OrAddr, err = net.ResolveTCPAddr("tcp", orPort)
	if err != nil {
		return info, EnvError(fmt.Sprintf("cannot resolve TOR_PT_ORPORT %q: %s", orPort, err.Error()))
	}

	info.BindAddrs, err = getServerBindAddrs(methodNames)
	if err != nil {
		return info, err
	}

	var extendedOrPort = getenv("TOR_PT_EXTENDED_SERVER_PORT")
	if extendedOrPort != "" {
		info.ExtendedOrAddr, err = net.ResolveTCPAddr("tcp", extendedOrPort)
		if err != nil {
			return info, EnvError(fmt.Sprintf("cannot resolve TOR_PT_EXTENDED_SERVER_PORT %q: %s", extendedOrPort, err.Error()))
		}
	}

	var authCookieFilename = getenv("TOR_PT_AUTH_COOKIE_FILE")
	if authCookieFilename != "" {
		info.AuthCookie, err = readAuthCookieFile(authCookieFilename)
		if err != nil {
			return info, EnvError(fmt.Sprintf("error reading TOR_PT_AUTH_COOKIE_FILE %q: %s", authCookieFilename, err.Error()))
		}
	}

	return info, nil
}

// See 217-ext-orport-auth.txt section 4.2.1.3.
func computeServerHash(info *ServerInfo, clientNonce, serverNonce []byte) []byte {
	h := hmac.New(sha256.New, info.AuthCookie)
	io.WriteString(h, "ExtORPort authentication server-to-client hash")
	h.Write(clientNonce)
	h.Write(serverNonce)
	return h.Sum([]byte{})
}

// See 217-ext-orport-auth.txt section 4.2.1.3.
func computeClientHash(info *ServerInfo, clientNonce, serverNonce []byte) []byte {
	h := hmac.New(sha256.New, info.AuthCookie)
	io.WriteString(h, "ExtORPort authentication client-to-server hash")
	h.Write(clientNonce)
	h.Write(serverNonce)
	return h.Sum([]byte{})
}

func extOrPortAuthenticate(s *net.TCPConn, info *ServerInfo) error {
	r := bufio.NewReader(s)

	// Read auth types. 217-ext-orport-auth.txt section 4.1.
	var authTypes [256]bool
	var count int
	for count = 0; count < 256; count++ {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b == 0 {
			break
		}
		authTypes[b] = true
	}
	if count >= 256 {
		return errors.New(fmt.Sprintf("read 256 auth types without seeing \\x00"))
	}

	// We support only type 1, SAFE_COOKIE.
	if !authTypes[1] {
		return errors.New(fmt.Sprintf("server didn't offer auth type 1"))
	}
	_, err := s.Write([]byte{1})
	if err != nil {
		return err
	}

	clientNonce := make([]byte, 32)
	clientHash := make([]byte, 32)
	serverNonce := make([]byte, 32)
	serverHash := make([]byte, 32)

	_, err = io.ReadFull(rand.Reader, clientNonce)
	if err != nil {
		return err
	}
	_, err = s.Write(clientNonce)
	if err != nil {
		return err
	}

	_, err = io.ReadFull(r, serverHash)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(r, serverNonce)
	if err != nil {
		return err
	}

	expectedServerHash := computeServerHash(info, clientNonce, serverNonce)
	if subtle.ConstantTimeCompare(serverHash, expectedServerHash) != 1 {
		return errors.New(fmt.Sprintf("mismatch in server hash"))
	}

	clientHash = computeClientHash(info, clientNonce, serverNonce)
	_, err = s.Write(clientHash)
	if err != nil {
		return err
	}

	status := make([]byte, 1)
	_, err = io.ReadFull(r, status)
	if err != nil {
		return err
	}
	if status[0] != 1 {
		return errors.New(fmt.Sprintf("server rejected authentication"))
	}

	if r.Buffered() != 0 {
		return errors.New(fmt.Sprintf("%d bytes left after extended OR port authentication", r.Buffered()))
	}

	return nil
}

// See section 3.1 of 196-transport-control-ports.txt.
const (
	extOrCmdDone      = 0x0000
	extOrCmdUserAddr  = 0x0001
	extOrCmdTransport = 0x0002
	extOrCmdOkay      = 0x1000
	extOrCmdDeny      = 0x1001
)

func extOrPortWriteCommand(s *net.TCPConn, cmd uint16, body []byte) error {
	var buf bytes.Buffer
	if len(body) > 65535 {
		return errors.New("command exceeds maximum length of 65535")
	}
	err := binary.Write(&buf, binary.BigEndian, cmd)
	if err != nil {
		return err
	}
	err = binary.Write(&buf, binary.BigEndian, uint16(len(body)))
	if err != nil {
		return err
	}
	err = binary.Write(&buf, binary.BigEndian, body)
	if err != nil {
		return err
	}
	_, err = s.Write(buf.Bytes())
	if err != nil {
		return err
	}

	return nil
}

// Send a USERADDR command on s. See section 3.1.2.1 of
// 196-transport-control-ports.txt.
func extOrPortSendUserAddr(s *net.TCPConn, conn net.Conn) error {
	return extOrPortWriteCommand(s, extOrCmdUserAddr, []byte(conn.RemoteAddr().String()))
}

// Send a TRANSPORT command on s. See section 3.1.2.2 of
// 196-transport-control-ports.txt.
func extOrPortSendTransport(s *net.TCPConn, methodName string) error {
	return extOrPortWriteCommand(s, extOrCmdTransport, []byte(methodName))
}

// Send a DONE command on s. See section 3.1 of 196-transport-control-ports.txt.
func extOrPortSendDone(s *net.TCPConn) error {
	return extOrPortWriteCommand(s, extOrCmdDone, []byte{})
}

func extOrPortRecvCommand(s *net.TCPConn) (cmd uint16, body []byte, err error) {
	var bodyLen uint16
	data := make([]byte, 4)

	_, err = io.ReadFull(s, data)
	if err != nil {
		return
	}
	buf := bytes.NewBuffer(data)
	err = binary.Read(buf, binary.BigEndian, &cmd)
	if err != nil {
		return
	}
	err = binary.Read(buf, binary.BigEndian, &bodyLen)
	if err != nil {
		return
	}
	body = make([]byte, bodyLen)
	_, err = io.ReadFull(s, body)
	if err != nil {
		return
	}

	return cmd, body, err
}

// Send USERADDR and TRANSPORT commands followed by a DONE command. Wait for an
// OKAY or DENY response command from the server. Returns nil if and only if
// OKAY is received.
func extOrPortSetup(s *net.TCPConn, conn net.Conn, methodName string) error {
	var err error

	err = extOrPortSendUserAddr(s, conn)
	if err != nil {
		return err
	}
	err = extOrPortSendTransport(s, methodName)
	if err != nil {
		return err
	}
	err = extOrPortSendDone(s)
	if err != nil {
		return err
	}
	cmd, _, err := extOrPortRecvCommand(s)
	if err != nil {
		return err
	}
	if cmd == extOrCmdDeny {
		return errors.New("server returned DENY after our USERADDR and DONE")
	} else if cmd != extOrCmdOkay {
		return errors.New(fmt.Sprintf("server returned unknown command 0x%04x after our USERADDR and DONE", cmd))
	}

	return nil
}

// Connect to info.ExtendedOrAddr if defined, or else info.OrAddr, and return an
// open *net.TCPConn. If connecting to the extended OR port, extended OR port
// authentication à la 217-ext-orport-auth.txt is done before returning; an
// error is returned if authentication fails.
func ConnectOr(info *ServerInfo, conn net.Conn, methodName string) (*net.TCPConn, error) {
	if info.ExtendedOrAddr == nil {
		return net.DialTCP("tcp", nil, info.OrAddr)
	}

	s, err := net.DialTCP("tcp", nil, info.ExtendedOrAddr)
	if err != nil {
		return nil, err
	}
	s.SetDeadline(time.Now().Add(5 * time.Second))
	err = extOrPortAuthenticate(s, info)
	if err != nil {
		s.Close()
		return nil, err
	}
	err = extOrPortSetup(s, conn, methodName)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.SetDeadline(time.Time{})

	return s, nil
}