package gophercloud

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
)

// DefaultUserAgent is the default User-Agent string set in the request header.
const DefaultUserAgent = "gophercloud/1.0.0"

// UserAgent represents a User-Agent header.
type UserAgent struct {
	// prepend is the slice of User-Agent strings to prepend to DefaultUserAgent.
	// All the strings to prepend are accumulated and prepended in the Join method.
	prepend []string
}

// Prepend prepends a user-defined string to the default User-Agent string. Users
// may pass in one or more strings to prepend.
func (ua *UserAgent) Prepend(s ...string) {
	ua.prepend = append(s, ua.prepend...)
}

// Join concatenates all the user-defined User-Agend strings with the default
// Gophercloud User-Agent string.
func (ua *UserAgent) Join() string {
	uaSlice := append(ua.prepend, DefaultUserAgent)
	return strings.Join(uaSlice, " ")
}

// ProviderClient stores details that are required to interact with any
// services within a specific provider's API.
//
// Generally, you acquire a ProviderClient by calling the NewClient method in
// the appropriate provider's child package, providing whatever authentication
// credentials are required.
type ProviderClient struct {
	// IdentityBase is the base URL used for a particular provider's identity
	// service - it will be used when issuing authenticatation requests. It
	// should point to the root resource of the identity service, not a specific
	// identity version.
	IdentityBase string

	// IdentityEndpoint is the identity endpoint. This may be a specific version
	// of the identity service. If this is the case, this endpoint is used rather
	// than querying versions first.
	IdentityEndpoint string

	// TokenID is the ID of the most recently issued valid token.
	TokenID string

	// EndpointLocator describes how this provider discovers the endpoints for
	// its constituent services.
	EndpointLocator EndpointLocator

	// HTTPClient allows users to interject arbitrary http, https, or other transit behaviors.
	HTTPClient http.Client

	// UserAgent represents the User-Agent header in the HTTP request.
	UserAgent UserAgent

	// ReauthFunc is the function used to re-authenticate the user if the request
	// fails with a 401 HTTP response code. This a needed because there may be multiple
	// authentication functions for different Identity service versions.
	ReauthFunc func() error
}

// AuthenticatedHeaders returns a map of HTTP headers that are common for all
// authenticated service requests.
func (client *ProviderClient) AuthenticatedHeaders() map[string]string {
	if client.TokenID == "" {
		return map[string]string{}
	}
	return map[string]string{"X-Auth-Token": client.TokenID}
}

// RequestOpts customizes the behavior of the provider.Request() method.
type RequestOpts struct {
	// JSONBody, if provided, will be encoded as JSON and used as the body of the HTTP request. The
	// content type of the request will default to "application/json" unless overridden by MoreHeaders.
	// It's an error to specify both a JSONBody and a RawBody.
	JSONBody interface{}
	// RawBody contains an io.ReadSeeker that will be consumed by the request directly. No content-type
	// will be set unless one is provided explicitly by MoreHeaders.
	RawBody io.ReadSeeker
	// JSONResponse, if provided, will be populated with the contents of the response body parsed as
	// JSON.
	JSONResponse interface{}
	// OkCodes contains a list of numeric HTTP status codes that should be interpreted as success. If
	// the response has a different code, an error will be returned.
	OkCodes []int
	// MoreHeaders specifies additional HTTP headers to be provide on the request. If a header is
	// provided with a blank value (""), that header will be *omitted* instead: use this to suppress
	// the default Accept header or an inferred Content-Type, for example.
	MoreHeaders map[string]string
	// ErrorType specifies the resource error type to return if an error is encountered.
	// This lets resources override default error messages based on the response status code.
	ErrorContext error
}

var applicationJSON = "application/json"

// Request performs an HTTP request using the ProviderClient's current HTTPClient. An authentication
// header will automatically be provided.
func (client *ProviderClient) Request(method, url string, options RequestOpts) (*http.Response, error) {
	var body io.ReadSeeker
	var contentType *string

	// Derive the content body by either encoding an arbitrary object as JSON, or by taking a provided
	// io.ReadSeeker as-is. Default the content-type to application/json.
	if options.JSONBody != nil {
		if options.RawBody != nil {
			panic("Please provide only one of JSONBody or RawBody to gophercloud.Request().")
		}

		rendered, err := json.Marshal(options.JSONBody)
		if err != nil {
			return nil, err
		}

		body = bytes.NewReader(rendered)
		contentType = &applicationJSON
	}

	if options.RawBody != nil {
		body = options.RawBody
	}

	// Construct the http.Request.
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	// Populate the request headers. Apply options.MoreHeaders last, to give the caller the chance to
	// modify or omit any header.
	if contentType != nil {
		req.Header.Set("Content-Type", *contentType)
	}
	req.Header.Set("Accept", applicationJSON)

	for k, v := range client.AuthenticatedHeaders() {
		req.Header.Add(k, v)
	}

	// Set the User-Agent header
	req.Header.Set("User-Agent", client.UserAgent.Join())

	if options.MoreHeaders != nil {
		for k, v := range options.MoreHeaders {
			if v != "" {
				req.Header.Set(k, v)
			} else {
				req.Header.Del(k)
			}
		}
	}

	// Issue the request.
	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	// Allow default OkCodes if none explicitly set
	if options.OkCodes == nil {
		options.OkCodes = defaultOkCodes(method)
	}

	// Validate the HTTP response status.
	var ok bool
	for _, code := range options.OkCodes {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		respErr := &UnexpectedResponseCodeError{
			BaseError: BaseError{
				Function: "*ProviderClient.Request",
			},
			URL:      url,
			Method:   method,
			Expected: options.OkCodes,
			Actual:   resp.StatusCode,
			Body:     body,
		}

		errType := options.ErrorContext
		switch resp.StatusCode {
		case http.StatusBadRequest:
			err = defaultError400{respErr}
			if error400er, ok := errType.(Error400er); ok {
				err = error400er.Error400(respErr)
			}
		case http.StatusUnauthorized:
			if client.ReauthFunc != nil {
				err = client.ReauthFunc()
				if err != nil {
					return nil, &ErrUnableToReauthenticate{
						respErr,
					}
				}
				if options.RawBody != nil {
					options.RawBody.Seek(0, 0)
				}
				resp, err = client.Request(method, url, options)
				if err != nil {
					switch err.(type) {
					case *UnexpectedResponseCodeError:
						return nil, &ErrErrorAfterReauthentication{err.(*UnexpectedResponseCodeError)}
					default:
						return nil, &ErrErrorAfterReauthentication{
							UnexpectedResponseCodeError: &UnexpectedResponseCodeError{
								BaseError: BaseError{
									OriginalError: err,
								},
							},
						}
					}
				}
				return resp, nil
			}
			err = defaultError401{respErr}
			if error401er, ok := errType.(Error401er); ok {
				err = error401er.Error401(respErr)
			}
		case http.StatusNotFound:
			err = defaultError404{respErr}
			if error404er, ok := errType.(Error404er); ok {
				err = error404er.Error404(respErr)
			}
		case http.StatusMethodNotAllowed:
			err = defaultError405{respErr}
			if error405er, ok := errType.(Error405er); ok {
				err = error405er.Error405(respErr)
			}
		case http.StatusRequestTimeout:
			err = defaultError408{respErr}
			if error408er, ok := errType.(Error408er); ok {
				err = error408er.Error408(respErr)
			}
		case 429:
			err = defaultError429{respErr}
			if error429er, ok := errType.(Error429er); ok {
				err = error429er.Error429(respErr)
			}
		case http.StatusInternalServerError:
			err = defaultError500{respErr}
			if error500er, ok := errType.(Error500er); ok {
				err = error500er.Error500(respErr)
			}
		case http.StatusServiceUnavailable:
			err = defaultError503{respErr}
			if error503er, ok := errType.(Error503er); ok {
				err = error503er.Error503(respErr)
			}
		}

		if err == nil {
			err = respErr
		}

		return resp, err
	}

	// Parse the response body as JSON, if requested to do so.
	if options.JSONResponse != nil {
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(options.JSONResponse)
	}

	return resp, nil
}

func defaultOkCodes(method string) []int {
	switch {
	case method == "GET":
		return []int{200}
	case method == "POST":
		return []int{201, 202}
	case method == "PUT":
		return []int{201, 202}
	case method == "DELETE":
		return []int{202, 204}
	}

	return []int{}
}

func (client *ProviderClient) Get(url string, JSONResponse *interface{}, opts *RequestOpts) (*http.Response, error) {
	if opts == nil {
		opts = &RequestOpts{}
	}
	if JSONResponse != nil {
		opts.JSONResponse = JSONResponse
	}
	return client.Request("GET", url, *opts)
}

func (client *ProviderClient) Post(url string, JSONBody interface{}, JSONResponse *interface{}, opts *RequestOpts) (*http.Response, error) {
	if opts == nil {
		opts = &RequestOpts{}
	}

	if v, ok := (JSONBody).(io.ReadSeeker); ok {
		opts.RawBody = v
	} else if JSONBody != nil {
		opts.JSONBody = JSONBody
	}

	if JSONResponse != nil {
		opts.JSONResponse = JSONResponse
	}

	return client.Request("POST", url, *opts)
}

func (client *ProviderClient) Put(url string, JSONBody interface{}, JSONResponse *interface{}, opts *RequestOpts) (*http.Response, error) {
	if opts == nil {
		opts = &RequestOpts{}
	}

	if v, ok := (JSONBody).(io.ReadSeeker); ok {
		opts.RawBody = v
	} else if JSONBody != nil {
		opts.JSONBody = JSONBody
	}

	if JSONResponse != nil {
		opts.JSONResponse = JSONResponse
	}

	return client.Request("PUT", url, *opts)
}

func (client *ProviderClient) Delete(url string, opts *RequestOpts) (*http.Response, error) {
	if opts == nil {
		opts = &RequestOpts{}
	}

	return client.Request("DELETE", url, *opts)
}
