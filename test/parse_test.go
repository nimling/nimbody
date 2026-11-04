package test

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/nimling/nimbody"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func TestBodyValidatorHasKey(t *testing.T) {
	tests := []struct {
		name     string
		body     nimbody.JSONBody
		key      string
		expected bool
	}{
		{"key exists", nimbody.JSONBody{"key": "value"}, "key", true},
		{"key does not exist", nimbody.JSONBody{"key": "value"}, "non-existent-key", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bv := &nimbody.BodyValidator{Body: test.body}
			actual := bv.HasKey(test.key)
			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestBodyValidatorHasValue(t *testing.T) {
	tests := []struct {
		name     string
		body     nimbody.JSONBody
		key      string
		expected bool
	}{
		{"key has value", nimbody.JSONBody{"key": "value"}, "key", true},
		{"key does not exist", nimbody.JSONBody{"key": "value"}, "non-existent-key", false},
		{"key has nil value", nimbody.JSONBody{"key": nil}, "key", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bv := &nimbody.BodyValidator{Body: test.body}
			actual := bv.HasValue(test.key)
			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestBodyValidatorForObject(t *testing.T) {
	tests := []struct {
		name     string
		body     nimbody.JSONBody
		key      string
		expected *nimbody.BodyValidator
	}{
		{"key exists and is a JSONBody", nimbody.JSONBody{"key": nimbody.JSONBody{"nested-key": "value"}}, "key", &nimbody.BodyValidator{Body: nimbody.JSONBody{"nested-key": "value"}}},
		{"key does not exist", nimbody.JSONBody{"key": "value"}, "non-existent-key", nil},
		{"key exists but is not a JSONBody", nimbody.JSONBody{"key": "value"}, "key", nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bv := &nimbody.BodyValidator{Body: test.body}
			actual := bv.ForObject(test.key)
			if test.expected != nil {
				assert.NotNil(t, actual)
				assert.Equal(t, test.expected.Body, actual.Body)
			} else {
				assert.Nil(t, actual)
			}
		})
	}
}

func TestBodyValidatorDeny(t *testing.T) {
	tests := []struct {
		name     string
		body     nimbody.JSONBody
		fields   []string
		expected []string
	}{
		{"no denied keys", nimbody.JSONBody{"name": "X"}, []string{"id"}, nil},
		{"exact match", nimbody.JSONBody{"id": "abc", "name": "X"}, []string{"id"}, []string{"id"}},
		{"multiple exact matches sorted", nimbody.JSONBody{"id": "a", "app_id": "b", "name": "X"}, []string{"id", "app_id"}, []string{"app_id", "id"}},
		{"wildcard suffix", nimbody.JSONBody{"sa_owner_id": 1, "sa_root_id": 2, "name": "X"}, []string{"sa_*"}, []string{"sa_owner_id", "sa_root_id"}},
		{"mixed exact and wildcard", nimbody.JSONBody{"id": "a", "sa_owner_id": 1, "name": "X"}, []string{"id", "sa_*"}, []string{"id", "sa_owner_id"}},
		{"empty body", nimbody.JSONBody{}, []string{"id"}, nil},
		{"empty fields", nimbody.JSONBody{"id": "a"}, []string{}, nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bv := &nimbody.BodyValidator{Body: test.body}
			actual := bv.Deny(test.fields...)
			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestBodyValidatorIsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		body     nimbody.JSONBody
		expected bool
	}{
		{"empty body", nimbody.JSONBody{}, true},
		{"non-empty body", nimbody.JSONBody{"key": "value"}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bv := &nimbody.BodyValidator{Body: test.body}
			actual := bv.IsEmpty()
			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected *nimbody.BodyValidator
		err      error
	}{
		{"valid JSON", "{\"key\":\"value\"}", &nimbody.BodyValidator{Body: nimbody.JSONBody{"key": "value"}}, nil},
		{"invalid JSON", "{key:\"value\"}", nil, errors.New("malformed request")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer([]byte(test.body)))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)

			actual, err := nimbody.Validator(c)

			if test.err != nil {
				assert.NotNil(t, err)
			} else {
				assert.Nil(t, err)
			}
			if test.expected != nil {
				assert.NotNil(t, actual)
				assert.Equal(t, test.expected.Body, actual.Body)
			} else {
				fmt.Println("here")

				assert.Nil(t, actual)
			}
		})
	}
}

func TestBind(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		i        interface{}
		expected *nimbody.BodyValidator
		err      error
	}{
		{"valid JSON and bind", "{\"key\":\"value\"}", &map[string]string{}, &nimbody.BodyValidator{Body: nimbody.JSONBody{"key": "value"}}, nil},
		{"invalid JSON", "{key\":\"value\"}", map[string]string{}, nil, errors.New("invalid character '}' looking for beginning of value")},
		{"bind error", "{\"key\":\"value\"}", struct{ UnexportedField string }{}, nil, errors.New("binding error")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer([]byte(test.body)))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)

			actual, err := nimbody.Bind(c, test.i)
			if test.err != nil {
				assert.NotNil(t, err)
			} else {
				assert.Nil(t, err)
			}
			if test.expected != nil {
				assert.NotNil(t, actual)
				assert.Equal(t, test.expected.Body, actual.Body)
			} else {
				assert.Nil(t, actual)
			}
		})
	}
}
