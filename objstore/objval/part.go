package objval

// Part represents the metadata from a single part from a multipart upload.
type Part struct {
	// ID is a unique identifier, which is used by each client when completing the multipart upload; this will be an
	// entity tag for some clients, and a generated key for others.
	ID string

	// Number is a number between 1-10,000 which can be used for ordering parts when a multipart upload is completed.
	//
	// NOTE: This field will not be populated in functions which fetch a list of parts from remote cloud providers.
	Number int

	// Size is the size of the part in bytes.
	Size int64
}

// Equal returns a boolean indicating whether this part is equal to the given part.
func (p Part) Equal(o Part) bool {
	return p.ID == o.ID && p.Number == o.Number && p.Size == o.Size
}
