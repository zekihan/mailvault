package sync

import "encoding/json"

// MarshalJSON preserves error messages rather than encoding opaque error
// implementations as {}. The operational Errors values remain intact.
func (r SyncResult) MarshalJSON() ([]byte, error) {
	type resultAlias SyncResult
	var messages []string
	if r.Errors != nil {
		messages = make([]string, 0, len(r.Errors))
		for _, err := range r.Errors {
			if err != nil {
				messages = append(messages, err.Error())
			}
		}
	}
	return json.Marshal(struct {
		resultAlias
		Errors []string `json:"Errors"`
	}{resultAlias: resultAlias(r), Errors: messages})
}
