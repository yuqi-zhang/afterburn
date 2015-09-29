package azure

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/coreos-metadata/src/config"
	"github.com/coreos/coreos-metadata/src/retry"
)

const (
	AgentName             = "com.coreos.metadata"
	FabricProtocolVersion = "2012-11-30"
)

func FetchMetadata() (config.Metadata, error) {
	addr, err := getFabricAddress()
	if err != nil {
		return config.Metadata{}, err
	}

	if err := assertFabricCompatible(addr, FabricProtocolVersion); err != nil {
		return config.Metadata{}, err
	}

	return fetchSharedConfig(addr)
}

func findLease() (*os.File, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("could not list interfaces: %v", err)
	}

	for _, iface := range ifaces {
		lease, err := os.Open(fmt.Sprintf("/run/systemd/netif/leases/%d", iface.Index))
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		} else {
			return lease, nil
		}
	}

	return nil, fmt.Errorf("could not find any leases")
}

func getFabricAddress() (net.IP, error) {
	lease, err := findLease()
	if err != nil {
		return nil, err
	}
	defer lease.Close()

	var rawEndpoint string
	line := bufio.NewScanner(lease)
	for line.Scan() {
		parts := strings.Split(line.Text(), "=")
		if parts[0] == "OPTION_245" && len(parts) == 2 {
			rawEndpoint = parts[1]
			break
		}
	}

	if len(rawEndpoint) == 0 || len(rawEndpoint) != 8 {
		return nil, fmt.Errorf("fabric endpoint not found in leases")
	}

	octets := make([]byte, 4)
	for i := 0; i < 4; i++ {
		octet, err := strconv.ParseUint(rawEndpoint[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		octets[i] = byte(octet)
	}

	return net.IPv4(octets[0], octets[1], octets[2], octets[3]), nil
}

func assertFabricCompatible(endpoint net.IP, desiredVersion string) error {
	body, err := retry.Client{
		InitialBackoff: time.Second,
		MaxBackoff:     time.Second * 5,
		MaxAttempts:    10,
	}.Get(fmt.Sprintf("http://%s/?comp=versions", endpoint))
	if err != nil {
		return fmt.Errorf("failed to fetch versions: %v", err)
	}

	versions := struct {
		Supported struct {
			Versions []string `xml:"Version"`
		}
		Preferred struct {
			Version string
		}
	}{}

	if err := xml.Unmarshal(body, &versions); err != nil {
		return fmt.Errorf("failed to unmarshal response: %v", err)
	}

	for _, version := range versions.Supported.Versions {
		if version == desiredVersion {
			return nil
		}
	}

	return fmt.Errorf("fabric version %s is not compatible", desiredVersion)
}

func fetchSharedConfig(endpoint net.IP) (config.Metadata, error) {
	client := retry.Client{
		InitialBackoff: time.Second,
		MaxBackoff:     time.Second * 5,
		MaxAttempts:    10,
		Header: map[string][]string{
			"x-ms-agent-name": {AgentName},
			"x-ms-version":    {FabricProtocolVersion},
			"Content-Type":    {"text/xml; charset=utf-8"},
		},
	}

	body, err := client.Get(fmt.Sprintf("http://%s/machine/?comp=goalstate", endpoint))
	if err != nil {
		return config.Metadata{}, fmt.Errorf("failed to fetch goal state: %v", err)
	}

	goal := struct {
		Container struct {
			RoleInstanceList struct {
				RoleInstance struct {
					Configuration struct {
						SharedConfig string
					}
				}
			}
		}
	}{}

	if err := xml.Unmarshal(body, &goal); err != nil {
		return config.Metadata{}, fmt.Errorf("failed to unmarshal response: %v", err)
	}

	body, err = client.Get(goal.Container.RoleInstanceList.RoleInstance.Configuration.SharedConfig)
	if err != nil {
		return config.Metadata{}, fmt.Errorf("failed to fetch shared config: %v", err)
	}

	sharedConfig := struct {
		Incarnation struct {
			Instance string `xml:"instance,attr"`
		}
		Instances struct {
			Instances []struct {
				Id             string `xml:"id,attr"`
				Address        string `xml:"address,attr"`
				InputEndpoints struct {
					Endpoints []struct {
						LoadBalancedPublicAddress string `xml:"loadBalancedPublicAddress,attr"`
					} `xml:"Endpoint"`
				}
			} `xml:"Instance"`
		}
	}{}

	if err := xml.Unmarshal(body, &sharedConfig); err != nil {
		return config.Metadata{}, err
	}

	metadata := config.Metadata{}
	for _, i := range sharedConfig.Instances.Instances {
		if i.Id == sharedConfig.Incarnation.Instance {
			metadata.LocalIPv4 = net.ParseIP(i.Address)

			for _, e := range i.InputEndpoints.Endpoints {
				host, _, err := net.SplitHostPort(e.LoadBalancedPublicAddress)
				if err == nil {
					metadata.PublicIPv4 = net.ParseIP(host)
					break
				}
			}

			break
		}
	}

	return metadata, nil
}
