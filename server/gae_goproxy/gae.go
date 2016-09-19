package gae

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"appengine"
	"appengine/urlfetch"
)

const (
	Version  = "1.0"
	Password = ""

	DefaultFetchMaxSize        = 1024 * 1024 * 4
	DefaultDeadline            = 20 * time.Second
	DefaultOverquotaDelay      = 4 * time.Second
	DefaultURLFetchClosedDelay = 1 * time.Second
	DefaultSSLVerify           = false
)

func IsBinary(b []byte) bool {
	if len(b) > 32 {
		b = b[:32]
	}
	if bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		return false
	}
	for _, c := range b {
		if c == '\n' {
			break
		}
		if c > 0x7f {
			return true
		}
	}
	return false
}

func IsGzip(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0x1f, 0x8b, 0x08, 0x00, 0x00})
}

func ReadRequest(r io.Reader) (req *http.Request, err error) {
	req = new(http.Request)

	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			err = fmt.Errorf("Invaild Request Line: %#v", line)
			return
		}

		req.Method = parts[0]
		req.RequestURI = parts[1]
		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1

		if req.URL, err = url.Parse(req.RequestURI); err != nil {
			return
		}
		req.Host = req.URL.Host

		req.Header = http.Header{}
	}

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		req.Header.Add(key, value)
	}

	if err = scanner.Err(); err != nil {
		// ignore
	}

	if cl := req.Header.Get("Content-Length"); cl != "" {
		if req.ContentLength, err = strconv.ParseInt(cl, 10, 64); err != nil {
			return
		}
	}

	req.Host = req.URL.Host
	if req.Host == "" {
		req.Host = req.Header.Get("Host")
	}

	return
}

func fmtError(c appengine.Context, err error) string {
	return fmt.Sprintf(`{
    "type": "appengine",
    "host": "%s",
    "software": "%s",
    "error": "%s"
}
`, appengine.DefaultVersionHostname(c), appengine.ServerSoftware(), err.Error())
}

func handlerError(c appengine.Context, rw http.ResponseWriter, err error, code int) {
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.BestCompression)

	data := fmtError(c, err)
	fmt.Fprintf(w, "HTTP/1.1 %d\r\n", code)
	fmt.Fprintf(w, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(w, "Content-Length: %d\r\n", len(data))
	io.WriteString(w, "\r\n")
	io.WriteString(w, data)
	w.Close()

	b0 := []byte{0, 0}
	binary.BigEndian.PutUint16(b0, uint16(b.Len()))

	rw.Header().Set("Content-Type", "image/gif")
	rw.Header().Set("Content-Length", strconv.Itoa(len(b0)+b.Len()))
	rw.WriteHeader(http.StatusOK)
	rw.Write(b0)
	rw.Write(b.Bytes())
}

func handler(rw http.ResponseWriter, r *http.Request) {
	var err error
	c := appengine.NewContext(r)
	c.Infof("Hanlde Request=%#v\n", r)

	var hdrLen uint16
	if err := binary.Read(r.Body, binary.BigEndian, &hdrLen); err != nil {
		c.Criticalf("binary.Read(&hdrLen) return %v", err)
		handlerError(c, rw, err, http.StatusBadRequest)
		return
	}

	req, err := ReadRequest(bufio.NewReader(flate.NewReader(&io.LimitedReader{R: r.Body, N: int64(hdrLen)})))
	if err != nil {
		c.Criticalf("http.ReadRequest(%#v) return %#v", r.Body, err)
		handlerError(c, rw, err, http.StatusBadRequest)
		return
	}

	req.Body = r.Body
	defer req.Body.Close()

	params := http.Header{}
	var paramPrefix string = http.CanonicalHeaderKey("X-UrlFetch-")
	for key, values := range req.Header {
		if strings.HasPrefix(key, paramPrefix) {
			params.Set(key, values[0])
		}
	}

	for key, _ := range params {
		req.Header.Del(key)
	}
	// req.Header.Del("X-Cloud-Trace-Context")

	c.Infof("Parsed Request=%#v\n", req)

	if Password != "" {
		password := params.Get("X-UrlFetch-Password")
		switch {
		case password == "":
			handlerError(c, rw, fmt.Errorf("No Password."), http.StatusForbidden)
			return
		case password != Password:
			handlerError(c, rw, fmt.Errorf("Wrong Password."), http.StatusForbidden)
			return
		}
	}

	deadline := DefaultDeadline
	if s := params.Get("X-UrlFetch-Deadline"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			deadline = time.Duration(n) * time.Second
		}
	}

	overquotaDelay := DefaultOverquotaDelay
	if s := params.Get("X-UrlFetch-OverquotaDelay"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			overquotaDelay = time.Duration(n) * time.Second
		}
	}

	urlfetchClosedDelay := DefaultURLFetchClosedDelay
	if s := params.Get("X-UrlFetch-URLFetchClosedDelay"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			urlfetchClosedDelay = time.Duration(n) * time.Second
		}
	}

	fetchMaxSize := DefaultFetchMaxSize
	if s := params.Get("X-UrlFetch-MaxSize"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			fetchMaxSize = n
		}
	}

	sslVerify := DefaultSSLVerify
	if s := params.Get("X-UrlFetch-SSLVerify"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			sslVerify = true
		}
	}

	var resp *http.Response
	for i := 0; i < 2; i++ {
		t := &urlfetch.Transport{
			Context:                       c,
			Deadline:                      deadline,
			AllowInvalidServerCertificate: !sslVerify,
		}

		resp, err = t.RoundTrip(req)
		if resp != nil && resp.Body != nil {
			if v := reflect.ValueOf(resp.Body).Elem().FieldByName("truncated"); v.IsValid() {
				if truncated := v.Bool(); truncated {
					resp.Body.Close()
					err = errors.New("URLFetchServiceError_RESPONSE_TOO_LARGE")
				}
			}
		}

		if err == nil {
			defer resp.Body.Close()
			break
		}

		message := err.Error()
		if strings.Contains(message, "RESPONSE_TOO_LARGE") {
			c.Warningf("URLFetchServiceError %T(%v) deadline=%v, url=%v", err, err, deadline, req.URL.String())
			if s := req.Header.Get("Range"); s != "" {
				if parts1 := strings.Split(s, "="); len(parts1) == 2 {
					if parts2 := strings.Split(parts1[1], "-"); len(parts2) == 2 {
						if start, err1 := strconv.Atoi(parts2[0]); err1 == nil {
							end, err1 := strconv.Atoi(parts2[1])
							if err1 != nil {
								req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+fetchMaxSize))
							} else {
								if end-start > fetchMaxSize {
									req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+fetchMaxSize))
								} else {
									req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
								}
							}
						}
					}
				}
			} else {
				req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", fetchMaxSize))
			}
		} else if strings.Contains(message, "Over quota") {
			c.Warningf("URLFetchServiceError %T(%v) deadline=%v, url=%v", err, err, deadline, req.URL.String())
			time.Sleep(overquotaDelay)
		} else if strings.Contains(message, "urlfetch: CLOSED") {
			c.Warningf("URLFetchServiceError %T(%v) deadline=%v, url=%v", err, err, deadline, req.URL.String())
			time.Sleep(urlfetchClosedDelay)
		} else {
			c.Errorf("URLFetchServiceError %T(%v) deadline=%v, url=%v", err, err, deadline, req.URL.String())
			break
		}
	}

	if err != nil {
		handlerError(c, rw, err, http.StatusBadGateway)
		return
	}

	// rewise resp.Header
	resp.Header.Del("Transfer-Encoding")
	if strings.ToLower(resp.Header.Get("Vary")) == "accept-encoding" {
		resp.Header.Del("Vary")
	}
	if resp.ContentLength > 0 {
		resp.Header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	if resp.Header.Get("Content-Encoding") == "" {
		if s := resp.Header.Get("Content-Type"); strings.HasPrefix(s, "text/") ||
			strings.HasPrefix(s, "application/json") ||
			strings.HasPrefix(s, "application/x-javascript") ||
			strings.HasPrefix(s, "application/javascript") {
			if v := reflect.ValueOf(resp.Body).Elem().FieldByName("content"); v.IsValid() {
				b := v.Bytes()
				switch {
				case IsGzip(b):
					resp.Header.Set("Content-Encoding", "gzip")
				case IsBinary(b) && strings.Contains(req.Header.Get("Accept-Encoding"), "deflate"):
					resp.Header.Set("Content-Encoding", "deflate")
				}
			}
		}
	}

	// Fix Set-Cookie header for python client
	// if r.Header.Get("User-Agent") == "" {
	// 	const cookieHeader string = "Set-Cookie"
	// 	if resp.Header.Get(cookieHeader) != "" {
	// 		cookies := make([]string, 0)
	// 		for _, c := range resp.Cookies() {
	// 			cookies = append(cookies, c.String())
	// 		}
	// 		resp.Header[cookieHeader] = []string{strings.Join(cookies, ", ")}
	// 	}
	// }

	c.Infof("Write Response=%#v\n", resp)

	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		handlerError(c, rw, err, http.StatusBadGateway)
		return
	}

	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", resp.StatusCode, resp.Status)
	resp.Header.Write(w)
	io.WriteString(w, "\r\n")
	w.Close()

	b0 := []byte{0, 0}
	binary.BigEndian.PutUint16(b0, uint16(b.Len()))

	rw.Header().Set("Content-Type", "image/gif")
	rw.Header().Set("Content-Length", strconv.FormatInt(int64(len(b0)+b.Len())+resp.ContentLength, 10))
	rw.WriteHeader(http.StatusOK)
	rw.Write(b0)
	io.Copy(rw, io.MultiReader(&b, resp.Body))
}

func favicon(rw http.ResponseWriter, r *http.Request) {
	rw.WriteHeader(http.StatusOK)
}

func robots(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(rw, "User-agent: *\nDisallow: /\n")
}

func root(rw http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	version, _ := strconv.ParseInt(strings.Split(appengine.VersionID(c), ".")[1], 10, 64)
	ctime := time.Unix(version/(1<<28), 0).Format(time.RFC3339)

	var latest string
	t := &urlfetch.Transport{Context: c}
	req, _ := http.NewRequest("GET", "https://github.com/phuslu/goproxy/commits/server.gae/gae", nil)
	resp, err := t.RoundTrip(req)
	if err != nil {
		latest = err.Error()
	} else {
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			latest = err.Error()
		} else {
			latest = regexp.MustCompile(`\d\d\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ`).FindString(string(data))
		}
	}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var message string
	switch {
	case latest == "":
		message = "unable check goproxy latest version, please try after 5 minutes."
	case latest <= ctime:
		message = "already update to latest."
	default:
		message = "please update this server"
	}
	fmt.Fprintf(rw, `{
	"server": "goproxy %s"
	"latest": "%s",
	"deploy": "%s",
	"message": "%s"
}
`, Version, latest, ctime, message)
}

func init() {
	http.HandleFunc("/_gh/", handler)
	http.HandleFunc("/favicon.ico", favicon)
	http.HandleFunc("/robots.txt", robots)
	http.HandleFunc("/", root)
}
