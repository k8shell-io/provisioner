package models

import (
	"fmt"
	"strings"

	"github.com/go-playground/validator/v10"
)

// ValidationError represents a user-friendly validation error
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
}

// ValidationErrors is a collection of validation errors
type Validator []ValidationError

func NewValidator(s interface{}) Validator {
	validate := validator.New()
	err := validate.Struct(s)
	if err == nil {
		return nil
	}

	var validationErrors Validator

	if validatorErrors, ok := err.(validator.ValidationErrors); ok {
		for _, fieldError := range validatorErrors {
			validationError := ValidationError{
				Field:   fieldError.Field(),
				Message: getValidationMessage(fieldError.Tag(), fieldError.Field(), fieldError.Param(), fmt.Sprintf("%v", fieldError.Value())),
				Value:   fmt.Sprintf("%v", fieldError.Value()),
			}
			validationErrors = append(validationErrors, validationError)
		}
	}
	if len(validationErrors) == 0 {
		return nil
	}
	return validationErrors
}

func (ve Validator) IsValid() bool {
	return len(ve) == 0
}

func (ve Validator) Errors() []ValidationError {
	return ve
}

func (ve Validator) ErrorMessages() string {
	if ve.IsValid() {
		return ""
	}
	var messages []string
	for _, err := range ve {
		messages = append(messages, fmt.Sprintf("- %s: %s", err.Field, err.Message))
	}
	return fmt.Sprintf("Validation errors:\n%s", strings.Join(messages, "\n"))
}

// getValidationMessage converts validation tags to user-friendly messages
func getValidationMessage(tag, field, param, value string) string {
	switch tag {
	case "required":
		return fmt.Sprintf("%s is required", field)
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s (value: %s)", field, strings.ReplaceAll(param, " ", ", "), value)
	case "min":
		return fmt.Sprintf("%s must be at least %s characters long (value: %s)", field, param, value)
	case "max":
		return fmt.Sprintf("%s must be no more than %s characters long (value: %s)", field, param, value)
	case "len":
		return fmt.Sprintf("%s must be exactly %s characters long (value: %s)", field, param, value)
	case "fqdn":
		return fmt.Sprintf("%s must be a valid domain name (value: %s)", field, value)
	case "uri":
		return fmt.Sprintf("%s must be a valid URI (value: %s)", field, value)
	case "startswith":
		return fmt.Sprintf("%s must start with '%s' (value: %s)", field, param, value)
	case "required_if":
		return fmt.Sprintf("%s is required when %s (value: %s)", field, param, value)
	default:
		return fmt.Sprintf("%s is invalid (validation: %s) (value: %s)", field, tag, value)
	}
}
