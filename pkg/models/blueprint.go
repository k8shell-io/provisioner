package models

// // Validate validates the blueprint and returns user-friendly errors
// func (b *Blueprint) Validate() Validator {
// 	return NewValidator(b)
// }

// func ValidateCustomBlueprint(blueprintYAML []byte) []string {
// 	var fullYAML map[string]interface{}
// 	if err := yaml.Unmarshal(blueprintYAML, &fullYAML); err != nil {
// 		return []string{
// 			fmt.Sprintf("Invalid YAML format: %v", err),
// 		}
// 	}

// 	var blueprintData, exists = fullYAML["blueprint"]
// 	if !exists {
// 		return []string{
// 			"Blueprint data is missing in the YAML",
// 		}
// 	}

// 	blueprintOnlyYAML, err := yaml.Marshal(blueprintData)
// 	if err != nil {
// 		return []string{
// 			fmt.Sprintf("Failed to process blueprint data: %v", err),
// 		}
// 	}

// 	var customBp CustomBlueprint
// 	decoder := yaml.NewDecoder(bytes.NewReader(blueprintOnlyYAML))
// 	decoder.KnownFields(true)
// 	if err := decoder.Decode(&customBp); err != nil {
// 		return []string{
// 			fmt.Sprintf("Failed to decode blueprint: %v", err),
// 		}
// 	}

// 	validate := validator.New()
// 	if err := validate.Struct(customBp); err != nil {
// 		var validationErrors []string
// 		for _, err := range err.(validator.ValidationErrors) {
// 			validationErrors = append(validationErrors,
// 				fmt.Sprintf("Field '%s' failed validation: %s", err.Field(), err.Tag()))
// 		}
// 		return validationErrors
// 	}
// 	return nil
// }
