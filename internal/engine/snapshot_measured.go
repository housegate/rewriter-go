package engine

// MeasuredIdentity is measured internally from the actual executable and sealed
// loaded image. It is not accepted as a constructor input or override.
type MeasuredIdentity struct {
	Platform         string
	ExecutableDigest string
	FFIDigest        string
}
