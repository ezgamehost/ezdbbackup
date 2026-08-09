// Package dump executes mysqldump without placing database credentials in its
// command-line arguments.
package dump

import "strconv"

// Request describes a validated mysqldump invocation.
type Request struct {
	Binary       string
	Host         string
	Port         int
	User         string
	Password     string
	AllDatabases bool
	Databases    []string
	ExtraArgs    []string
}

// Args returns the deterministic mysqldump arguments for a backup request.
func Args(req Request) []string {
	args := []string{
		"--host=" + req.Host,
		"--port=" + strconv.Itoa(req.Port),
		"--user=" + req.User,
	}
	args = append(args, req.ExtraArgs...)
	if req.AllDatabases {
		return append(args, "--all-databases")
	}
	args = append(args, "--databases")
	return append(args, req.Databases...)
}

// ProbeArgs returns the mysqldump arguments used to verify connectivity and
// permissions without emitting data or DDL.
func ProbeArgs(req Request) []string {
	args := Args(req)
	return append(args, "--no-data", "--no-create-info", "--skip-triggers")
}
