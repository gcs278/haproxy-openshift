package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

type Backend struct {
	HostAddr    string      `json:"host_addr"`
	Name        string      `json:"name"`
	TrafficType TrafficType `json:"traffic_type"`
}

type BoundBackend struct {
	Backend

	Port int `json:"port"`
}

type BackendsByTrafficType map[TrafficType][]Backend

func (b BoundBackend) URL() string {
	return fmt.Sprintf("%s://%s:%v/1024.html", b.TrafficType.Scheme(), b.HostAddr, b.Port)
}

func mustResolveCurrentHost() string {
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	net.LookupIP(hostname)
	return hostname
}

// TODO; we want anything but 127.0.0.1 || ::1 returned.
func getOutboundIPAddr() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}

func createFile(path string, data []byte) error {
	dirname := filepath.Dir(path)
	if err := os.MkdirAll(dirname, 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	return f.Close()
}
