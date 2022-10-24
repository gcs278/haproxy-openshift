package main

type HAProxyGenCmd struct {
	HTTPPort  int  `default:"8080"`
	HTTPSPort int  `default:"8443"`
	Maxconn   int  `default:"0"`
	Nthreads  int  `default:"4"`
	StatsPort int  `default:"1936"`
	TLSReuse  bool `default:"true"`
}

type MakeCertsCmd struct {
	Regenerate bool `default:"false" short:"r"`
}

type PrintHostsCmd struct {
	Domain string `short:"d" default:"localdomain"`
}

type ServeBackendsCmd struct{}

type ServeBackendCmd struct {
	Args []string `arg:""`
}

type VersionCmd struct{}
