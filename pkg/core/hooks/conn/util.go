package conn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var (
	realTimeOffset uint64
)

// convertUnixNanoToTime takes a Unix timestamp in nanoseconds as a uint64 and returns the corresponding time.Time
func convertUnixNanoToTime(unixNano uint64) time.Time {
	// Unix time is the number of seconds since January 1, 1970 UTC,
	// so convert nanoseconds to seconds for time.Unix function
	seconds := int64(unixNano / uint64(time.Second))
	nanoRemainder := int64(unixNano % uint64(time.Second))
	return time.Unix(seconds, nanoRemainder)
}

func isFiltered(logger *zap.Logger, req *http.Request, opts models.IncomingOptions) bool {
	dstPort := 0
	var err error
	if p := req.URL.Port(); p != "" {
		dstPort, err = strconv.Atoi(p)
		if err != nil {
			utils.LogError(logger, err, "failed to obtain destination port from request")
			return false
		}
	}

	var passThrough bool

	type cond struct {
		eligible bool
		match    bool
	}

	for _, filter := range opts.Filters {

		//  1. bypass rule
		bypassEligible := !(filter.BypassRule.Host == "" &&
			filter.BypassRule.Path == "" &&
			filter.BypassRule.Port == 0)

		opts := models.OutgoingOptions{Rules: []config.BypassRule{filter.BypassRule}}
		byPassMatch := utils.IsPassThrough(logger, req, uint(dstPort), opts)

		//  2. URL-method rule
		urlMethodEligible := len(filter.URLMethods) > 0
		urlMethodMatch := false
		if urlMethodEligible {
			for _, m := range filter.URLMethods {
				if m == req.Method {
					urlMethodMatch = true
					break
				}
			}
		}

		//  3. header rule
		headerEligible := len(filter.Headers) > 0
		headerMatch := false
		if headerEligible {
			for key, vals := range filter.Headers {
				rx, err := regexp.Compile(vals)
				if err != nil {
					utils.LogError(logger, err, "bad header regex")
					continue
				}
				for _, v := range req.Header.Values(key) {
					if rx.MatchString(v) {
						headerMatch = true
						break
					}
				}
				if headerMatch {
					break
				}
			}
		}

		conds := []cond{
			{bypassEligible, byPassMatch},
			{urlMethodEligible, urlMethodMatch},
			{headerEligible, headerMatch},
		}

		switch filter.MatchType {
		case config.AND:
			pass := true
			seen := false
			for _, c := range conds {
				if !c.eligible {
					continue
				} // ignore ineligible ones
				seen = true
				if !c.match {
					pass = false
					break
				}
			}
			if seen && pass {
				passThrough = true
				return passThrough
			}

		case config.OR:
			fallthrough
		default:
			for _, c := range conds {
				if c.eligible && c.match {
					passThrough = true
					return passThrough
				}
			}
		}
	}

	return passThrough
}

//// LogAny appends input of any type to a logs.txt file in the current directory
//func LogAny(value string) error {
//
//	logMessage := value
//
//	// Add a timestamp to the log message
//	timestamp := time.Now().Format("2006-01-02 15:04:05")
//	logLine := fmt.Sprintf("%s - %s\n", timestamp, logMessage)
//
//	// Open logs.txt in append mode, create it if it doesn't exist
//	file, err := os.OpenFile("logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
//	if err != nil {
//		return err
//	}
//	defer file.Close()
//
//	// Write the log line to the file
//	_, err = file.WriteString(logLine)
//	if err != nil {
//		return err
//	}
//
//	return nil
//}

func Capture(_ context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http request body")
		return
	}

	if req.Header.Get("Content-Encoding") != "" {
		reqBody, err = pkg.Decompress(logger, req.Header.Get("Content-Encoding"), reqBody)
		if err != nil {
			utils.LogError(logger, err, "failed to decompress the request body")
			return
		}
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the http response body")
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http response body")
		return
	}

	if isFiltered(logger, req, opts) {
		logger.Debug("The request is a filtered request")
		return
	}
	var formData []models.FormData
	if contentType := req.Header.Get("Content-Type"); strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, ";")
		if len(parts) > 1 {
			req.Header.Set("Content-Type", strings.TrimSpace(parts[0]))
		}
		formData = extractFormData(logger, reqBody, contentType)
		reqBody = []byte{}
	} else if contentType := req.Header.Get("Content-Type"); contentType == "application/x-www-form-urlencoded" {
		decodedBody, err := url.QueryUnescape(string(reqBody))
		if err != nil {
			utils.LogError(logger, err, "failed to decode the url-encoded request body")
			return
		}
		reqBody = []byte(decodedBody)
	}

	if resp.Header.Get("Content-Encoding") != "" {
		respBody, err = pkg.Decompress(logger, resp.Header.Get("Content-Encoding"), respBody)
		if err != nil {
			utils.LogError(logger, err, "failed to decompress the response body")
			return
		}
	}

	t <- &models.TestCase{
		Version: models.GetVersion(),
		Name:    pkg.ToYamlHTTPHeader(req.Header)["Keploy-Test-Name"],
		Kind:    models.HTTP,
		Created: time.Now().Unix(),
		HTTPReq: models.HTTPReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			// URL:        req.URL.String(),
			// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
			URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			//  URL: string(b),
			Form:      formData,
			Header:    pkg.ToYamlHTTPHeader(req.Header),
			Body:      string(reqBody),
			URLParams: pkg.URLParams(req),
			Timestamp: reqTimeTest,
		},
		HTTPResp: models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHTTPHeader(resp.Header),
			Body:          string(respBody),
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise: map[string][]string{},
		// Mocks: mocks,
	}
}

func extractFormData(logger *zap.Logger, body []byte, contentType string) []models.FormData {
	boundary := ""
	if strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, "boundary=")
		if len(parts) > 1 {
			boundary = strings.TrimSpace(parts[1])
		} else {
			utils.LogError(logger, nil, "Invalid multipart/form-data content type")
			return nil
		}
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var formData []models.FormData

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			utils.LogError(logger, err, "Error reading part")
			continue
		}
		key := part.FormName()
		if key == "" {
			continue
		}

		value, err := io.ReadAll(part)
		if err != nil {
			utils.LogError(logger, err, "Error reading part value")
			continue
		}

		formData = append(formData, models.FormData{
			Key:    key,
			Values: []string{string(value)},
		})
	}

	return formData
}

// CaptureGRPC captures a gRPC request/response pair and sends it to the test case channel
func CaptureGRPC(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, http2Stream *pkg.HTTP2Stream) {
	if http2Stream == nil {
		logger.Error("Stream is nil")
		return
	}

	if http2Stream.GRPCReq == nil || http2Stream.GRPCResp == nil {
		logger.Error("gRPC request or response is nil")
		return
	}

	// Create test case from stream data
	testCase := &models.TestCase{
		Version:  models.GetVersion(),
		Name:     http2Stream.GRPCReq.Headers.OrdinaryHeaders["Keploy-Test-Name"],
		Kind:     models.GRPC_EXPORT,
		Created:  time.Now().Unix(),
		GrpcReq:  *http2Stream.GRPCReq,
		GrpcResp: *http2Stream.GRPCResp,
		Noise:    map[string][]string{},
	}

	select {
	case <-ctx.Done():
		return
	case t <- testCase:
		logger.Debug("Captured gRPC test case",
			zap.String("path", http2Stream.GRPCReq.Headers.PseudoHeaders[":path"]))
	}
}
