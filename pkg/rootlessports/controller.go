//go:build !windows
// +build !windows

package rootlessports

import (
	"context"
	"strconv"
	"time"

	"github.com/k3s-io/k3s/pkg/rootless"
	coreClients "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rootless-containers/rootlesskit/pkg/api/client"
	"github.com/rootless-containers/rootlesskit/pkg/port"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	all = "_all_"
)

func Register(ctx context.Context, serviceController coreClients.ServiceController, enabled bool, httpsPort int) error {
	var (
		err            error
		rootlessClient client.Client
	)

	if rootless.Sock == "" {
		return nil
	}

	for i := 0; i < 30; i++ {
		rootlessClient, err = client.New(rootless.Sock)
		if err == nil {
			break
		} else {
			logrus.Infof("Waiting for rootless API socket %s: %v", rootless.Sock, err)
			time.Sleep(1 * time.Second)
		}
	}
	if err != nil {
		return err
	}

	h := &handler{
		enabled:        enabled,
		rootlessClient: rootlessClient,
		serviceClient:  serviceController,
		serviceCache:   serviceController.Cache(),
		httpsPort:      httpsPort,
		ctx:            ctx,
	}
	serviceController.OnChange(ctx, "rootlessports", h.serviceChanged)
	serviceController.Enqueue("", all)

	return nil
}

type handler struct {
	enabled        bool
	rootlessClient client.Client
	serviceClient  coreClients.ServiceController
	serviceCache   coreClients.ServiceCache
	httpsPort      int
	ctx            context.Context
}

func (h *handler) serviceChanged(key string, svc *v1.Service) (*v1.Service, error) {
	if key != all {
		h.serviceClient.Enqueue("", all)
		return svc, nil
	}

	ports, err := h.rootlessClient.PortManager().ListPorts(h.ctx)
	if err != nil {
		return svc, err
	}

	boundPorts := map[int]int{}
	for _, port := range ports {
		boundPorts[port.Spec.ParentPort] = port.ID
	}

	toBindPort, err := h.toBindPorts()
	if err != nil {
		return svc, err
	}

	for bindPort, childBindPort := range toBindPort {
		if _, ok := boundPorts[bindPort]; ok {
			logrus.Debugf("Parent port %d to child already bound", bindPort)
			delete(boundPorts, bindPort)
			continue
		}

		status, err := h.rootlessClient.PortManager().AddPort(h.ctx, port.Spec{
			Proto:      "tcp",
			ParentPort: bindPort,
			ChildPort:  childBindPort,
		})
		if err != nil {
			return svc, err
		}

		logrus.Infof("Bound parent port %s:%d to child namespace port %d", status.Spec.ParentIP,
			status.Spec.ParentPort, status.Spec.ChildPort)
	}

	for bindPort, id := range boundPorts {
		if err := h.rootlessClient.PortManager().RemovePort(h.ctx, id); err != nil {
			return svc, err
		}

		logrus.Infof("Removed parent port %d to child namespace", bindPort)
	}

	return svc, nil
}

func (h *handler) toBindPorts() (map[int]int, error) {
	svcs, err := h.serviceCache.List("", labels.Everything())
	if err != nil {
		return nil, err
	}

	toBindPorts := map[int]int{
		h.httpsPort: h.httpsPort,
	}

	if !h.enabled {
		return toBindPorts, nil
	}

	for _, svc := range svcs {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP == "" {
				continue
			}

			unprivilegedPortStart := determineUnprivilegedPortStart()
			for _, port := range svc.Spec.Ports {
				if port.Protocol != v1.ProtocolTCP {
					continue
				}

				if port.Port != 0 {
					if port.Port < unprivilegedPortStart {
						toBindPorts[10000+int(port.Port)] = int(port.Port)
					} else {
						toBindPorts[int(port.Port)] = int(port.Port)
					}
				}
			}
		}
	}

	return toBindPorts, nil
}

func determineUnprivilegedPortStart() int32 {
	var port int32 = 1024
	unprivilegedPortStart, err := rootless.ReadSysctl("net.ipv4.ip_unprivileged_port_start")
	if err != nil {
		logrus.Debugf("Found net.ipv4.ip_unprivileged_port_start=%s", unprivilegedPortStart)
		parsedPort, err := strconv.ParseInt(unprivilegedPortStart, 10, 8)
		if err != nil {
			port = int32(parsedPort)
		} else {
			logrus.Errorf("Error when parsing port: %s", err)
		}
	} else {
		logrus.Errorf("Error when retrieving net.ipv4.ip_unprivileged_port_start: %s", err)
	}

	logrus.Infof("Using port number %d unprivilged port start", port)

	return port
}
