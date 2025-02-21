package cws

// TODO: Replace this with the CWS' client
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pkg/errors"
)

const (
	// SessionHeader is the header key for a session.
	SessionHeader = "Token"
	// HeaderAuthorization is the HTTP header Authorization.
	HeaderAuthorization = "Authorization"
	// AuthorizationBearer is the bearer HTTP authorization type.
	AuthorizationBearer = "BEARER"
	defaultHTTPTimeout  = time.Minute
)

// CreateInstallationRequest needed parameters to create a new
// installation in CWS
type CreateInstallationRequest struct {
	CustomerID             string `json:"customer_id"`
	RequestedWorkspaceName string `json:"workspace_name"`
	Version                string `json:"version"`
	Image                  string `json:"image"`
	GroupID                string `json:"group_id"`
	APILock                bool   `json:"api_lock"`
}

// User model that represents a CWS user
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// Customer model that represents a CWS customer
type Customer struct {
	ID        string `json:"id"`
	CreatorID string `json:"creator_id"`
}

// SignupResponse model returned after the signup
// is called succesfully
type SignupResponse struct {
	User     *User     `json:"user"`
	Customer *Customer `json:"customer"`
}

// CreateInstallationResponse model returned after a
// installation is created successfully
type CreateInstallationResponse struct {
	InstallationID string `json:"installationId"`
	Token          string `json:"token"`
}

// RegisterStripeWebhookResponse model returned after webhook endpoint is registered in Stripe by the test portal
type RegisterStripeWebhookResponse struct {
	Secret string `json:"secret"`
}

// Installation model that represents a CWS installation
type Installation struct {
	ID             string
	State          string
	SubscriptionID string
	CustomerID     string
}

type apiError struct {
	Message string `json:"message"`
}

// Client CWS client to perform actions the against CWS service
type Client struct {
	httpClient          *http.Client
	publicURL           string
	internalURL         string
	headers             map[string]string
	internalOnlyHeaders map[string]string
}

// NewClient returns a new CWS client
func NewClient(publicAPIHost, internalAPIHost, apiKey string) *Client {
	internalHeaders := map[string]string{"X-MM-Api-Key": apiKey}
	return &Client{
		httpClient:          &http.Client{Timeout: defaultHTTPTimeout},
		publicURL:           publicAPIHost,
		internalURL:         internalAPIHost,
		headers:             make(map[string]string),
		internalOnlyHeaders: internalHeaders,
	}
}

// Login perform a login in the CWS API
func (c *Client) Login(email, password string) (*User, error) {
	parameters := fmt.Sprintf(`{"email": "%s", "password": "%s"}`, email, password)
	resp, err := c.makeRequest(c.publicURL, http.MethodPost, "/api/v1/users/login", []byte(parameters), false)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to log into CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to log into CWS API")
		}
		var user *User
		err = json.Unmarshal(body, &user)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to log into CWS API")
		}
		c.headers[HeaderAuthorization] = fmt.Sprintf("%s %s", AuthorizationBearer, resp.Header.Get(SessionHeader))
		return user, nil
	}
	return nil, readAPIError(resp)
}

// SignUp sign up a new user in the CWS service
func (c *Client) SignUp(email, password string) (*SignupResponse, error) {
	parameters := fmt.Sprintf(`{"email": "%s", "password": "%s"}`, email, password)
	resp, err := c.makeRequest(c.publicURL, http.MethodPost, "/api/v1/users/signup", []byte(parameters), false)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to sign up into CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusCreated:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to sign up into CWS API")
		}
		var signupResponse *SignupResponse
		err = json.Unmarshal(body, &signupResponse)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to sign up into CWS API")
		}
		c.headers[HeaderAuthorization] = fmt.Sprintf("%s %s", AuthorizationBearer, resp.Header.Get(SessionHeader))
		return signupResponse, nil
	}
	return nil, readAPIError(resp)
}

// GetMyCustomers returns the customers asociated to the logged
// user
func (c *Client) GetMyCustomers() ([]*Customer, error) {
	resp, err := c.makeRequest(c.publicURL, http.MethodGet, "/api/v1/customers", nil, false)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to request my customers from CWS API")
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to request my customers from CWS API")
		}
		var customers []*Customer
		err = json.Unmarshal(body, &customers)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to request my customers from CWS API")
		}
		return customers, nil
	}
	return nil, readAPIError(resp)
}

// VerifyUser verifies the user passed a parameter
func (c *Client) VerifyUser(userID string) error {
	path := fmt.Sprintf("/api/v1/internal/users/%s/verify", userID)
	resp, err := c.makeRequest(c.internalURL, http.MethodPost, path, nil, true)
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

// CreateInstallation creates a new installation in CWS using the parameters paseed in the installation request
func (c *Client) CreateInstallation(installationRequest *CreateInstallationRequest) (*CreateInstallationResponse, error) {
	requestBytes, err := json.Marshal(installationRequest)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal create installation request")
	}
	resp, err := c.makeRequest(c.internalURL, http.MethodPost, "/api/v1/internal/installation", requestBytes, true)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to create CWS installation")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to create CWS installation")
		}
		var installationResponse *CreateInstallationResponse
		err = json.Unmarshal(body, &installationResponse)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to create CWS installation")
		}
		return installationResponse, nil
	}
	return nil, readAPIError(resp)
}

// DeleteInstallation deleted the installation passed as parameter
func (c *Client) DeleteInstallation(installationID string) error {
	path := fmt.Sprintf("/api/v1/internal/installation/%s", installationID)
	resp, err := c.makeRequest(c.internalURL, http.MethodDelete, path, nil, true)
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

// RegisterStripeWebhook Calls test portal's internal API to register a new webhook endpoint in Stripe
func (c *Client) RegisterStripeWebhook(url, owner string) (string, error) {
	path := fmt.Sprintf("/api/v1/internal/tests/spinwick/register_stripe_webhook")
	resp, err := c.makeRequest(c.internalURL, http.MethodPost, path, []byte(fmt.Sprintf(`{"url": "%s", "owner": "%s"}`, url, owner)), true)
	if err != nil {
		return "", errors.Wrap(err, "error trying to register stripe webhook")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusCreated:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", errors.Wrap(err, "error trying to register stripe webhook")
		}
		var response *RegisterStripeWebhookResponse
		err = json.Unmarshal(body, &response)
		if err != nil {
			return "", errors.Wrap(err, "error trying to register stripe webhook")
		}
		return response.Secret, nil
	}

	return "", readAPIError(resp)
}

// DeleteStripeWebhook Calls test portal's internal API to delete a webhook endpoint in Stripe
func (c *Client) DeleteStripeWebhook(owner string) error {
	path := fmt.Sprintf("/api/v1/internal/tests/spinwick/stripe_webhook/%s", owner)
	resp, err := c.makeRequest(c.internalURL, http.MethodDelete, path, nil, true)
	if err != nil {
		return errors.Wrap(err, "error trying to delete stripe webhook")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	}

	return errors.New("Error deleting Stripe Webhook")
}

// GetInstallations returns the installations associated that belongs to the logged user
func (c *Client) GetInstallations() ([]*Installation, error) {
	resp, err := c.makeRequest(c.publicURL, http.MethodGet, "/api/v1/installations", nil, false)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to get CWS installations")
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to get CWS installation")
		}
		var installations []*Installation
		err = json.Unmarshal(body, &installations)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to get CWS installation")
		}
		return installations, nil
	case http.StatusNotFound:
		return []*Installation{}, nil
	}
	return nil, readAPIError(resp)
}

func (c *Client) makeRequest(host, method, path string, body []byte, isInternal bool) (*http.Response, error) {
	req, err := http.NewRequest(method, host+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Add(k, v)
	}
	if isInternal {
		for k, v := range c.internalOnlyHeaders {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func closeBody(r *http.Response) {
	if r.Body != nil {
		_, _ = io.ReadAll(r.Body)
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
