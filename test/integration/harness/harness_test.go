package harness

import (
	"os/exec"
	"testing"
)

func TestPostgresStopRemovesAnonymousVolume(t *testing.T) {
	postgres := StartPostgres(t)
	volume := dockerOutput(t, "inspect", "--format", "{{range .Mounts}}{{.Name}}{{end}}", postgres.container)
	if volume == "" {
		t.Fatal("disposable PostgreSQL has no anonymous data volume")
	}

	if err := postgres.Stop(); err != nil {
		t.Fatalf("stop disposable PostgreSQL: %v", err)
	}
	if err := exec.Command("docker", "volume", "inspect", volume).Run(); err == nil {
		_ = exec.Command("docker", "volume", "rm", "--force", volume).Run()
		t.Fatalf("disposable PostgreSQL volume %s remains after Stop", volume)
	}
}
