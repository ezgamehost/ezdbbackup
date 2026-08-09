package buildinfo

var Version = "dev"

func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
