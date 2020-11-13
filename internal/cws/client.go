package cws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/pkg/errors"
)

const defaultHTTPTimeout = time.Minute

type CreateInstallationRequest struct {
	CustomerID             string
	RequestedWorkspaceName string
	Version                string
	GroupID                string
	APILock                bool
}

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type Customer struct {
	ID        string `json:"id"`
	CreatorID string `json:"creator_id"`
}

type SignupResponse struct {
	User     *User     `json:"user"`
	Customer *Customer `json:"customer"`
}

type CreateInstallationResponse struct {
	InstallationID string `json:"installationId"`
	Token          string `json:"token"`
}

type Installation struct {
	ID             string
	State          string
	SubscriptionID string
	CustomerID     string
}

type apiError struct {
	Message string `json:"message"`
}

type Client struct {
	httpClient  *http.Client
	publicURL   string
	internalURL string
}

func NewClient(publicAPIHost, internalAPIHost string) *Client {
	return &Client{
		httpClient:  &http.Client{Timeout: defaultHTTPTimeout},
		publicURL:   publicAPIHost,
		internalURL: internalAPIHost,
	}
}

// Login perform a login in the CWS API
func (c *Client) Login(email, password string) (*User, error) {
	parameters := fmt.Sprintf(`{"email": "%s", "password": "%s"}`, email, password)
	resp, err := c.makeRequest(c.publicURL, "POST", "/api/v1/login", []byte(parameters))
	if err != nil {
		return nil, errors.Wrap(err, "error trying to log into CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := ioutil.ReadAll(resp.Body)
		var user *User
		err = json.Unmarshal(body, &user)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to log into CWS API")
		}
		return user, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) SignUp(email, password string) (*SignupResponse, error) {
	parameters := fmt.Sprintf(`{"email": "%s", "password": "%s"}`, email, password)
	resp, err := c.makeRequest(c.publicURL, "POST", "/api/v1/signup", []byte(parameters))
	if err != nil {
		return nil, errors.Wrap(err, "error trying to sign up into CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := ioutil.ReadAll(resp.Body)
		var signupResponse *SignupResponse
		err = json.Unmarshal(body, &signupResponse)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to sign up into CWS API")
		}
		return signupResponse, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) GetMyCustomers() ([]*Customer, error) {
	resp, err := c.makeRequest(c.publicURL, "GET", "/api/v1/customers", nil)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to request my customers from CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := ioutil.ReadAll(resp.Body)
		var customers []*Customer
		err = json.Unmarshal(body, &customers)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to request my customers from CWS API")
		}
		return customers, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) VerifyUser(userID string) error {
	path := fmt.Sprintf("/api/v1/internal/users/%s/verify", userID)
	resp, err := c.makeRequest(c.internalURL, "POST", path, nil)
	if err != nil {
		return errors.Wrap(err, "error trying to request my customers from CWS API")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	}
	return readAPIError(resp)
}

func (c *Client) CreateInstallation(installationRequest *CreateInstallationRequest) (*CreateInstallationResponse, error) {
	requestBytes, err := json.Marshal(installationRequest)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal create installation request")
	}
	resp, err := c.makeRequest(c.internalURL, "POST", "/api/v1/internal/installation", requestBytes)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to create CWS installation")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := ioutil.ReadAll(resp.Body)
		var installationResponse *CreateInstallationResponse
		err = json.Unmarshal(body, &installationResponse)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to create CWS installation")
		}
		return installationResponse, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) DeleteInstallation(installationID string) error {
	//TODO Request to internal API
	path := fmt.Sprintf("/api/v1/internal/installation/%s", installationID)
	resp, err := c.makeRequest(c.internalURL, "POST", path, nil)
	if err != nil {
		return errors.Wrapf(err, "error trying to delete CWS installation %s", installationID)
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	}
	return readAPIError(resp)
}

func (c *Client) GetInstallations() ([]*Installation, error) {
	resp, err := c.makeRequest(c.internalURL, "POST", "/api/v1/installations", nil)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to get CWS installations")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := ioutil.ReadAll(resp.Body)
		var installations []*Installation
		err = json.Unmarshal(body, &installations)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to get CWS installation")
		}
		return installations, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) makeRequest(host, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, host+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func closeBody(r *http.Response) {
	if r.Body != nil {
		_, _ = ioutil.ReadAll(r.Body)
		_ = r.Body.Close()
	}
}

func apiErrorFromReader(reader io.Reader) (*apiError, error) {
	apiErr := apiError{}
	decoder := json.NewDecoder(reader)
	err := decoder.Decode(&apiErr)
	if err != nil && err != io.EOF {
		return nil, err
	}

	return &apiErr, nil
}

func readAPIError(resp *http.Response) error {
	apiErr, err := apiErrorFromReader(resp.Body)
	if err != nil || apiErr == nil {
		return errors.Errorf("failed with status code %d for url %s", resp.StatusCode, resp.Request.URL)
	}

	return errors.Wrapf(errors.New(apiErr.Message), "failed with status code %d", resp.StatusCode)
}