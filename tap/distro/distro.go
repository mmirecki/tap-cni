package distro

import (
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/mmirecki/tap-cni/tap/conf"
)

type Distro interface {
	CreateLink(tmpName string, conf *conf.NetConf, netns ns.NetNS) error
}
