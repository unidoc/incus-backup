package nbd

import "net"

// netPipeSilent returns a connection whose peer never speaks.
func netPipeSilent() (net.Conn, net.Conn) { return net.Pipe() }
