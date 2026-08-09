package fixture

// The local name "shared" is bound here to a different import path than the
// one cross_file_alias.go binds it to, while the selector text written in
// both files is character-for-character the same. Only per-file resolution
// can tell the two apart, and the registry is keyed by what that resolution
// produces rather than by the text.
import shared "example.com/othertypes"

type OtherPackagePayload struct {
	Status shared.Status `json:"status"`
}
