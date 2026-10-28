# Auswertung: PoC vereinheitlichter Panko-Server und vereinfachte Installation

Branch `poc/unified-server-dev-install`, Auftrag: `POC.md` (lokale Aufgabenbeschreibung, nicht im Repository).
Keine Veröffentlichung, kein Merge nach `main`. Stand: 2026-10-28.

## Ergebnis

Der Ansatz ist technisch tragfähig. Eine gemeinsame Binary `panko-server` startet alle
Laufzeitrollen; Metacontroller v4.17.6 läuft als festgelegte Go-Abhängigkeit in der
Controller-Rolle und ruft den bestehenden Hook über Loopback-HTTP auf. Ein versioniertes
Helm-Chart ist der einzige Installationsweg für `panko install`, `panko dev up`, `make deploy`
und GitOps. `panko dev up` erzeugt aus einem beliebigen Verzeichnis ohne Checkout, Make oder
Image-Build ein dediziertes k3d-Lab, ohne Hintergrundprozess und ohne die globale Kubeconfig
zu verändern. Die bestehenden E2E-Suiten bestehen unverändert in ihrem fachlichen Umfang
(nur Image- und Adressanpassungen der Testfixtures).

## Was funktioniert und wie es reproduziert wird

| Bereich | Umsetzung | Nachweis |
| --- | --- | --- |
| Gemeinsame Binary | `cmd/panko-server`, Rollen in `internal/roles`: `gateway`, `controller`, `endpoint`, `mirror`, `runner`, `agent`, `setup` | Unit-Tests `internal/roles`; alle Rollen laufen im Lab, `agent` im Remote-Runner-E2E |
| Eingebetteter Metacontroller | `internal/roles/controller.go`, `server.New` aus Metacontroller v4.17.6, Hook auf `127.0.0.1:8090` | Lab: `database-controller` wird synchronisiert, Lebenszyklus vollständig |
| Getrennte ServiceAccounts | Controller `panko-controller`, Gateway `platform-gateway`, Endpoint `runner-endpoint`, Setup `panko-setup`, Runner `tofu-runner` je Team; Mirrors ohne Token | `internal/chart`-Tests, `kubectl auth can-i` im Lab, `test/rbac_e2e.py` |
| Helm-Chart | `charts/panko` (Version `0.1.0-poc.1`): CRDs, RBAC, Rollen, Setup-Job, Lab-PostgreSQL/S3 | `make lint-chart`, `internal/chart`-Tests |
| `panko install` | `internal/install`: Ziel ausweisen, Rechte und Konflikte prüfen, CRDs anwenden, Helm-SDK-Install/Upgrade, Readiness | Lab über `make deploy` (CI-Weg) und `panko dev up` |
| `panko dev up/status/down` | `internal/devlab`: Docker, k3d (privat mit Prüfsumme), eigener Cluster und Kubeconfig, Port-Mapping statt Port-Forward | manueller Lauf ohne Checkout, Unit-Tests mit gefälschtem `docker`/`k3d` |
| GitOps | `examples/gitops`: Werte, Argo CD, Flux, vorgerenderte Manifeste; [gitops.md](gitops.md) | Rendering reproduzierbar, keine Secrets; Argo CD/Flux nicht angewendet |
| `panko admin` | bisherige `panko-admin`-Befehle, zusätzlich `--config cluster` | `test/admin_e2e.py` u. a. über den Kompatibilitäts-Wrapper `admin` |

Reproduktion:

```sh
# Unit-, Chart- und Lint-Prüfungen
make test-unit lint lint-go
# Lab ohne Checkout (Images einmalig bauen, z. B. auf einem anderen Rechner)
docker build -t panko-server:dev -f Dockerfile.server .
docker build -t panko-module-mirror:dev -f cluster/Dockerfile.module-mirror .
go build -o /tmp/panko ./cmd/panko
cd "$(mktemp -d)" && /tmp/panko dev up --server-image panko-server:dev --module-mirror-image panko-module-mirror:dev
# CI-Weg mit E2E-Suiten
make cluster-up build deploy CLUSTER_NAME=panko-poc-e2e
make test-shard SHARD=a   # ebenso b und c
```

### Geprüfte Abläufe

Alle Läufe am 2026-10-28 auf macOS ARM64 mit Docker 29.2, k3d 5.9.0, k3s v1.35.5, jeweils in
eigenen Wegwerf-Clustern; vorhandene Cluster und Kubeconfigs blieben unverändert.

| Prüfung | Ergebnis |
| --- | --- |
| `make test-unit lint lint-go` (Go-Tests mit Race Detector, `helm lint --strict`, reproduzierbares Rendering ohne Secrets, gofmt, vet, ruff) | bestanden |
| `node --test .github/scripts/release-gate.test.cjs` | 20/20 bestanden |
| `panko dev up` aus einem Verzeichnis ohne Checkout, `PATH` ohne k3d: Zustimmung abgelehnt → Abbruch ohne Änderung; mit `--install-k3d` private Installation, SHA-256 geprüft | bestanden |
| Lebenszyklus im Dev-Lab: `doctor` ohne `panko.json`, Create mit absichtlich falschem PostgreSQL-Passwort → `Failed provider`, Korrektur, `retry --wait` → Ready (Versuch 2), `connect`, `delete --wait` → Ressource, State und Connection-Secret entfernt | bestanden |
| Neustart: `k3d cluster stop`, erneutes `dev up` startet und aktualisiert das Lab (Revision 3); State-Fingerprint, Token-Datei und Ressource unverändert | bestanden |
| `dev down` mit falscher Bestätigung → keine Änderung; mit Lab-Namen → nur dieses Lab gelöscht | bestanden |
| Globale `~/.kube/config` | Inhalt und Kontexte unverändert; im ersten Lauf schrieb `k3d cluster delete` die Datei jedoch neu (gleicher Inhalt, neue Änderungszeit). Behoben: k3d läuft mit einer lab-eigenen `KUBECONFIG`; spätere Läufe ließen die Datei unberührt |
| CI-Weg `make cluster-up build deploy` (`panko install` in einen vorhandenen k3d-Cluster), danach E2E-Shards a, b, c (RBAC, Lebenszyklus, Gateway, CLI, UI, Egress, State-Backends, Fehler/Retry, Drift, Admin-Migration, Remote-Runner, Runner-Smoke, Provider-Mirror, Observability) | alle bestanden (~16 min) |
| Erneutes `make deploy` auf dem getesteten Lab: CRDs angewendet, Setup-Job mit gleichem Namen neu erzeugt und abgeschlossen, Revision 2 | bestanden |
| `kubectl auth can-i` je Rolle: Controller/Gateway/Setup/Endpoint ohne fremde Secrets, Runner nur im eigenen State-Namespace, Setup ohne Workloads, Endpoint nur `runs/status` | wie vorgesehen |
| Migration einer v0.0.1-Installation (`make bootstrap build deploy` von `main`) mit Ressource, siehe [Migration](#migration) | bestanden nach drei Korrekturen |
| Abgebrochenes `panko install` (Prozess während des Helm-Wartens beendet): nächster Lauf markiert die hängende Revision als fehlgeschlagen und installiert erneut | bestanden |
| Abgebrochenes `panko dev up` (SIGTERM während des Helm-Wartens), erneutes `dev up` ohne Port-Angabe: Lab und Port wiederverwendet, Revision `pending-install` fortgesetzt, danach `dev status`, Lebenszyklus in `team-b`, `dev down --yes`; globale Kubeconfig vor und nach dem Lauf byte-identisch (Änderungszeit, Größe, SHA-256) | bestanden |

## Metacontroller-Einbettung und Integrationsgrenzen

Der Prototyp (separates Go-Modul, nur `server.New` aufrufen) war der erste Schritt; er hat
die folgenden Grenzen gezeigt, die in die Umsetzung eingeflossen sind:

- **Modulpfad.** Metacontrollers `go.mod` heißt `module metacontroller`, die Tags `v4.x` haben
  kein `/v4`-Suffix. `go get` ist deshalb nicht möglich. Eingebunden wird es mit
  `require metacontroller v0.0.0` und `replace metacontroller =>
  github.com/metacontroller/metacontroller v1.5.21-0.20261015191212-be3f3f973ddc`, der
  Pseudo-Version für den Commit `be3f3f97` des Tags `v4.17.6`; `go.sum` legt den Inhalt fest.
  Renovate erkennt diese Bindung nicht; Updates sind manuell (Commit des neuen Tags auflösen).
- **Abhängigkeiten.** Metacontroller v4.17.6 und Helm v4.3.1 verwenden dieselben
  `k8s.io/*` v0.37.1 und controller-runtime v0.25.2 wie Panko. Künftig müssen alle drei im
  Gleichschritt aktualisiert werden; ein Versatz der Kubernetes-Bibliotheken blockiert Updates.
- **API-Fläche.** Eingebettet wird `pkg/server.New(ctx, options.Configuration)`, das einen
  controller-runtime-Manager liefert. Nicht über die Konfiguration erreichbar sind u. a.
  `LeaderElectionReleaseOnCancel` und eigene Health-Checks; Panko setzt
  `HealthProbeBindAddress: "0"` und bedient `/healthz`/`/readyz` selbst (Readiness = Hook-Listener
  aktiv und Cache-Sync des Managers). Metacontroller nutzt globale Logger (`logging.Logger`,
  klog, controller-runtime); es kann nur eine Instanz je Prozess geben, Logs sind
  zap-JSON auf stderr neben Pankos JSON-Logs auf stdout.
- **Hook-Anbindung.** Der Hook bleibt unverändert und wird über `http://127.0.0.1:8090` im selben
  Pod aufgerufen. `/sync`, `/customize` und `/finalize` sind damit nicht mehr aus dem
  Pod-Netz erreichbar; der Port 8080 bedient nur `/healthz`, `/readyz` und `/metrics`. Eine
  direkte In-Process-Anbindung ohne HTTP wäre möglich, wurde aber bewusst nicht umgesetzt.
- **Fremde Controller.** Die eingebettete Instanz verwaltet nur CompositeController mit dem
  Label `platform.example.com/controller=panko` (`--target-label-selector`). Ein separat
  laufender Metacontroller ohne Selektor würde Pankos Controller trotzdem ebenfalls
  bedienen; `panko install` verweigert deshalb einen Metacontroller im Namespace
  `metacontroller`.
- **RBAC.** Statt der Wildcard-Rechte des Metacontroller-Charts hat die Controller-Rolle eine
  explizite ClusterRole. Im Lab fehlte zunächst `list/watch` auf `namespaces`
  (Metacontroller beobachtet Namespaces für Selektoren verwandter Ressourcen); ergänzt.
- **CRDs.** Die Metacontroller-CRDs stammen unverändert aus v4.17.6 und liegen in
  `charts/panko/crds/metacontroller.yaml`. Gehören sie einer anderen Installation, lässt
  `panko install` sie unverändert.
- **Lizenz.** Metacontroller steht unter Apache-2.0 wie Panko.

## Bekannte Einschränkungen und offene Entscheidungen

- **Images und Chart sind nicht veröffentlicht.** Der Release-Workflow baut `panko-server`
  und `panko-module-mirror`, wurde aber nicht ausgeführt; ein OCI-Chart fehlt. Für GitOps
  müssen Images in eine eigene Registry geschoben werden.
- **Namen.** Die Controller-Rolle läuft weiterhin im Deployment `infra-runners/database-hook`,
  weil Pause/Resume, Sicherung, Migrationen und Tests diesen Namen verwenden. Eine
  Umbenennung (z. B. `panko-controller`) ist offen.
- **Hook im Pod mit API-Token.** Der Hook stellt keine API-Anfragen, läuft aber jetzt im
  selben Prozess wie Metacontroller und damit mit dessen ServiceAccount. Die frühere
  Prozessgrenze (Hook ohne Token) entfällt. Offen: Hook als zweiter Container im Pod
  (getrennter Prozess, weiterhin Loopback) oder bewusste Annahme.
- **Feste Secret-Namen.** Vorhandene Secrets werden über feste Namen referenziert, nicht über
  frei wählbare Werte (`secrets.generate=false` prüft sie). Konfigurierbare Namen würden
  Änderungen in Gateway, Runner, Endpoint und Admin-Befehlen erfordern.
- **Setup-Job statt Helm-Hooks.** Credentials entstehen in einem normalen Job, dessen Name einen
  Digest seiner Eingaben trägt; Pods warten bis dahin in `ContainerCreating`. Helm-Hooks
  hätten Namespaces und RBAC außerhalb des Releases benötigt. Bei GitOps läuft der Job nur
  bei geänderten Eingaben; ein gelöschtes Credential entsteht erst nach Löschen des Jobs
  neu. `panko install` startet ihn bei jedem Lauf neu.
- **Zertifikatserneuerung** erneuert die Secrets, startet Mirror und Endpoint aber nicht neu.
- **Gateway-RBAC** wird aus `gateway.exposedResources` gerendert. Zur Laufzeit ergänzte CRDs
  brauchen weiterhin `panko admin gateway-rbac`; ein Upgrade setzt diese Ergänzungen zurück.
- **Modul-Rollout.** Controller und Modul-Mirror rollen im selben Upgrade aus; ein weniger als
  eine Minute alter Commit kann für die ersten Jobs fehlen (`module_fetch`, dann `retry`).
  Das alte Skript startete erst den Mirror und dann den Hook.
- **Plattformen.** `panko dev` unterstützt macOS und Linux mit Docker; Podman, Windows und
  VM-Verwaltung sind ausgeschlossen. k3d wird bei Bedarf in v5.9.0 installiert; die
  Prüfsummen sind im CLI festgelegt und müssen bei einem k3d-Update angepasst werden.
- **Argo CD und Flux** sind nur als Beispiele gerendert und geprüft, nicht gegen laufende
  Controller angewendet.
- **Binary-Größe.** `panko` wächst durch das Helm-SDK auf rund 98 MB, `panko-admin` auf
  68 MB, `panko-server` auf 49 MB (Image 58 MB).
- **`test/portability_e2e.py`** (Python/Go-Übergänge mit gesicherten Images) ist nicht Teil der
  Suiten und für den eingebetteten Controller nicht mehr aussagekräftig.

## Auswirkungen auf Berechtigungen, Abhängigkeiten und Updates

- **Berechtigungen.** Neu: `panko-controller` (ClusterRole: API-Gruppe
  `platform.example.com` vollständig, `metacontroller.k8s.io`, Jobs, lesend Pods, Namespaces
  und CRDs, Events; Leases nur in `infra-runners`; keine Secrets) ersetzt die breiten
  Standardrechte des Metacontroller-Charts. Neu: `panko-setup` (Secrets erstellen und
  aktualisieren in `infra-*`, erstellen in Team-Namespaces; ConfigMaps und NetworkPolicies in
  Team-Namespaces; Services lesend für Egress-Ziele; keine Ressourcen, keine Workloads).
  Unverändert: Gateway (keine Secrets), Endpoint, Runner je Team mit getrenntem State.
- **Installation.** `panko install` braucht Cluster-Administratorrechte für CRDs, ClusterRoles
  und Namespaces und prüft sie vorab mit `SelfSubjectAccessReview`.
- **Abhängigkeiten.** Neu: Helm SDK v4.3.1 (nur im CLI), Metacontroller v4.17.6 mit
  controller-runtime, zap und Prometheus-Client (nur in `panko-server`), `k8s.io/cli-runtime`.
  OpenTofu und git bleiben externe Werkzeuge (Engine-Image, git-sync, Agent-Image).
- **Updates.** Metacontroller-Updates sind ein Code-Update von Panko (Pseudo-Version in
  `go.mod`, CRDs in `crds/`), kein separates Chart-Update mehr. CRDs aktualisiert Helm nie;
  `panko install` wendet sie vor jedem Upgrade an, GitOps über `CreateReplace` bzw.
  `ServerSideApply`. Das Chart behält Namespaces, Daten-PVCs und StatefulSets bei
  `helm uninstall` und GitOps-Pruning.

## Migration

Bestehende Installationen aus `make deploy` (v0.0.1 und `main` vor diesem PoC) haben
Objekte ohne Helm-Eigentum und einen separaten Metacontroller im Namespace `metacontroller`.

Geprüfter Ablauf (Lab aus `main`, Database `orders` im Zustand Ready):

```sh
panko admin --kubeconfig KC --config panko.json pause          # Hook und Gateway auf 0, laufende Jobs abwarten
helm --kubeconfig KC uninstall metacontroller -n metacontroller
kubectl --kubeconfig KC delete namespace metacontroller
panko install --kubeconfig KC --config panko.json --values VALUES --tokens-file .local/team-tokens.json \
  --take-ownership --yes                                        # Images müssen im Cluster verfügbar sein
panko admin --kubeconfig KC --config panko.json resume
kubectl --kubeconfig KC -n infra-backends delete job s3-onboard --ignore-not-found   # Altlast, nicht im Chart
```

`--take-ownership` übernimmt Namespaces, RBAC, Services, ConfigMaps, StatefulSets, die
CompositeController-Definition und die Metacontroller-CRDs in das Helm-Release und erzwingt dabei
Konflikte mit dem Feldmanager von `kubectl apply`. Die zustandslosen Deployments
(`database-hook`, `platform-gateway`, `runner-endpoint`, `provider-mirror`, `module-mirror`)
werden gelöscht und vom Chart neu angelegt, weil Server-Side Apply fremd verwaltete Felder (etwa
den alten `hook`-Container) nicht entfernen kann. Daten liegen in Secrets, StatefulSets und PVCs
und bleiben unverändert; der Finalizer `metacontroller.io/compositecontroller-database-controller`
hat denselben Namen, der eingebettete Controller setzt die Verwaltung nahtlos fort.

Ergebnis: State, Connection-Secret, Gateway-Tokens, UID, `lastAppliedHash` und Job-Zahl vor und
nach der Migration identisch, kein erneuter Apply. Danach führte eine Spec-Änderung zu einem
erfolgreichen Apply (Generation 2) und das Löschen zu Destroy samt State-Bereinigung.

Der Test deckte drei Fehler auf, die behoben sind: (1) Server-Side-Apply-Konflikte mit
`kubectl-client-side-apply` – Adoption setzt jetzt `ForceConflicts`; (2) `strategy: Recreate`
ist mit dem vorhandenen `rollingUpdate`-Block ungültig – der Controller nutzt `RollingUpdate`
mit `maxSurge: 0`; (3) übernommene Deployments behielten den alten Container – sie werden jetzt
ersetzt (Erkennung über die `last-applied-configuration`-Annotation von `kubectl apply`).

Nicht geprüft: Installationen mit privatem SSH-Modul, Egress-Regeln oder Runner-Pools, sowie
Installationen aus der Zeit vor der State-Isolation (zuerst mit v0.0.1 migrieren, siehe
[Betrieb](operations.de.md#vorhandene-installation-migrieren)). Vor der Migration
`make backup` mit dem alten Stand ausführen.

## Verbleibende Arbeit bis zu einer veröffentlichbaren Version

1. Release-Workflow: `panko-server`, `panko-module-mirror` und Agent-Image mit festen Tags,
   Chart als OCI-Artefakt mit passender `appVersion`, CLI-Archive mit eingebettetem Chart.
2. Argo CD und Flux in CI gegen einen Wegwerf-Cluster anwenden (z. B. kind mit Flux).
3. Entscheidung zum Hook-Prozess (eigener Container) und zur Umbenennung von `database-hook`.
4. Konfigurierbare Namen vorhandener Secrets oder eine dokumentierte External-Secrets-Vorlage.
5. Neustart von Mirror und Endpoint nach Zertifikatserneuerung (z. B. Checksum-Annotation
   aus dem Setup-Job oder Neuladen des Zertifikats im Prozess).
6. Modul-Rollout: Controller erst nach Bereitschaft des Mirrors mit der neuen Revision.
7. Automatische Prüfung der Metacontroller-Version (Renovate-Regel oder CI-Check für
   `replace` und `crds/metacontroller.yaml`).
8. Migration als ein Befehl (Pause, Entfernen des separaten Metacontrollers, Aufräumen der
   Altobjekte, Resume) und als CI-Test mit einem v0.0.1-Lab.
9. Leader-Election und mehrere Controller-Replikate im Lab prüfen (vorhanden, nicht getestet).
10. Dokumentation der Release-Version aktualisieren (Quickstart verweist für 0.0.1 auf den Tag).
