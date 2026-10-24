package runproto

// Fixed messages per stage; raw OpenTofu output stays in the Job log.
var messages = map[string]string{
	"binding":          "The bound state backend does not belong to this resource",
	"credentials":      "Credentials for the provider or state backend could not be loaded",
	"module":           "The module could not be prepared",
	"backend_init":     "OpenTofu could not initialize the state backend or providers",
	"backend_mismatch": "OpenTofu initialized a different state backend than the binding",
	"checkpoint":       "The state backend no longer holds the state Panko last recorded",
	"provider":         "OpenTofu failed while changing infrastructure",
	"outputs":          "Module connection outputs could not be read or published; a retry publishes them again",
	"state_record":     "The resulting state could not be read or recorded",
	"cleanup":          "State cleanup after destroy failed",
	"interrupted":      "The runner was interrupted",
	"migration":        "The state migration failed",
}

// Message returns a fixed server-side message, or empty for an unknown code.
func Message(code string) string {
	if code == "" {
		return "Completed"
	}
	return messages[code]
}
