# Go-Portierung: Aufbau, Upgrade und Rückwechsel

Die Laufzeitkomponenten verwenden Go 1.27.3 und client-go v0.36.5. Metacontroller,
CRDs und HCL behalten ihre bisherigen Aufgaben. Python bleibt für die externen
E2E-Tests und optionale Werkzeuge wie die Logo-Erzeugung verfügbar.

## Aufbau und Entwicklung

| Einstiegspunkt | Implementierung | Aufgabe |
| --- | --- | --- |
| `cmd/gateway` | `internal/gateway` | HTTP, Team-Tokens, dynamische CRD-Discovery, OpenAPI und Discovery-Cache |
| `cmd/hook` | `internal/hook` | Metacontroller-Reconciliation, Jobs, Status, Finalisierung und Metriken |
| `cmd/runner` | `internal/runner` | Snapshot prüfen, technische Credentials laden, OpenTofu ausführen und State aufräumen |
| `cmd/admin` | `internal/admin` | RBAC erzeugen, Auth-Secrets initialisieren, Deployment prüfen und Legacy-State migrieren |
| gemeinsam | `internal/kube`, `internal/resource`, `internal/jsonx`, `internal/serve` | client-go-Transport, Snapshots, kompatible JSON-Hashes und geordnetes HTTP-Herunterfahren |

`make build-go` erzeugt die lokalen Binaries unter `.local/bin/`. Administratoren
verwenden normalerweise `make bootstrap`, `make deploy`, `make gateway-rbac` und
`make migrate-state`; diese Targets bauen die Verwaltungsbefehle bei Bedarf.
Cluster-Zugriffe des Admin-Binaries verlangen eine explizite Kubeconfig oder
`KUBECONFIG`. Runtime-Dienste authentifizieren sich mit ihrem ServiceAccount.

`make test-unit` bzw. `make test-go` führt die Go-Tests mit Race Detector aus.
`make lint-go` prüft Formatierung und `go vet`; `make lint` prüft die verbliebenen
Python-Werkzeuge. Die Python-E2E-Tests greifen ausschließlich über HTTP, Kubernetes
und PostgreSQL auf die Anwendung zu. Der Runner-Smoke-Test importiert keine
Implementierungsfunktionen und kann gegen beide Runner-Sprachen laufen.

Die festen Python-Referenzfälle unter `testdata/` prüfen HTTP-Antworten, OpenAPI,
RBAC, Reconciliation, Hashes und Job-Namen. Beim Vergleich von Job-Snapshots zählt
der JSON-Inhalt; Whitespace und Objektschlüsselreihenfolge im Transportstring sind
unerheblich. Der für Hashes verwendete JSON-Bytestrom muss dagegen exakt passen.
Die Referenzfälle werden bei fachlichen Änderungen bewusst geprüft und angepasst;
sie werden nicht aus der Go-Implementierung als neuer Sollwert erzeugt.

Go wird in `mise.toml` und beiden Runtime-Dockerfiles festgelegt; die Go-Direktive
steht in `go.mod`. Bei Abhängigkeitsänderungen `go.mod` und `go.sum` gemeinsam prüfen,
`mise exec -- go mod tidy` und `mise exec -- go mod verify` ausführen und danach
die Tests starten. Go-Caches liegen für lokale Make-Aufrufe unter `.local/`.

Die Dockerfiles bauen für Linux AMD64 oder ARM64 über `TARGETOS`/`TARGETARCH`.
Zum Zeitpunkt der Portierung enthielt der Runner OpenTofu, das Modul und einen
Provider-Mirror ohne Registry-Fallback. Inzwischen setzen Jobs Engine und Modul beim
Start zusammen; siehe [Engine und Modul](operations.de.md#engine-und-modul).
Keines der Images enthält eine Python-Runtime.

## Getestete Übergänge

Die Portierung erfolgte komponentenweise im lokalen k3d-Labor:

1. Go-Gateway mit Python-Hook und Python-Runner: vollständige E2E-Suite bestanden.
2. Go-Hook mit Go-Gateway und Python-Runner: vollständige E2E-Suite bestanden.
   Ein aktiver Python-Job blieb beim Hook-Wechsel Go → Python → Go mit identischer
   UID und unveränderter Pod-Vorlage erhalten und wurde regulär abgeschlossen.
3. Go-Runner: Ein von Python erzeugter State wurde mit Go, Python und erneut Go
   aktualisiert. Datenbankname, Rolle und Zugangsdaten blieben identisch; der
   abschließende Go-Destroy entfernte Infrastruktur, Connection-Secret, State und Lock.
4. Die Go-RBAC-Ausgabe entspricht semantisch den gespeicherten Python-Referenzen.
   Migrationstests im Cluster prüfen belegte Locks, abweichende Ziel-Secrets,
   Erhalt aller Quellen bei Konflikten, Wiederaufnahme, private Backups und Token-Erhalt.
5. Upgrade einer reinen Python-Installation per `make build deploy`: Der Go-Hook
   übernahm eine von Python bereitgestellte Ressource ohne zusätzlichen Apply und mit
   byteidentischem Status, State und Zugangsdaten, aktualisierte und zerstörte sie
   anschließend. Danach bestand die vollständige E2E-Suite im aktualisierten Lab.

Die Runtime-Tests der Portierung liefen auf Linux ARM64; die vier Go-Kommandos wurden
zusätzlich für Linux AMD64 cross-kompiliert. Inzwischen läuft die vollständige E2E-Suite
auch in GitHub Actions auf Linux AMD64 (Nachweis in [#13](https://github.com/hingst-it/panko/issues/13)). Die entpackten Images
schrumpften von 117 auf 29,9 MB (Gateway), 78,9 auf 23,9 MB (Hook) und 280 auf
230 MB (Runner, überwiegend OpenTofu und Provider-Mirror). Startzeit und
Speicherverbrauch wurden nicht gemessen.

Lokale Protokolle liegen unter `.local/go-stage*-*.log` und `.local/go-upgrade-*`; abschließende Nachweise
stehen in den Issues des Meilensteins [Go port](https://github.com/hingst-it/panko/milestone/2?closed=1), zusammengefasst in [#13](https://github.com/hingst-it/panko/issues/13).

## Gesicherte Python-Versionen

Die folgenden lokalen ARM64-Images sind vor dem Überschreiben der `:dev`-Tags
gesichert worden. Sie müssen aufbewahrt oder vor einem Rückwechsel erneut
bereitgestellt werden; sie wurden nicht in eine Registry veröffentlicht.

| Image | Image-ID |
| --- | --- |
| `panko-gateway:python-baseline-20261023` | `sha256:ca457f89291716367c768b57401fe3d97611905a4ce0f8f9ce55c265c276388f` |
| `panko-hook:python-baseline-20261023` | `sha256:9dcb8622f3288995cdf9f88a7aa433e02047dcc5da220a8958ae15b1ee3ee768` |
| `panko-runner:python-baseline-20261023` | `sha256:dfcce6d46b4cbfea61a1f430e87930ec380e974147d3e66a30687825b4e0a6ae` |

Die ursprünglichen Quellen liegen lokal unter `.local/python-baseline/`. Diese
Sicherung gehört nicht zum ausgelieferten Go-Quellbaum. Für andere Umgebungen
müssen die entsprechenden vorherigen Release-Images verfügbar sein.

## Rückwechsel ohne State-Migration

Vor einem Wechsel aktuelle Deployments, Jobs und Image-Referenzen aufnehmen.
Den bestehenden State und seine Team-Namespaces unverändert lassen. Die Portierung
erfordert keine State-Migration; die separate Legacy-Backend-Migration ist in der
[Betriebsanleitung](operations.de.md) beschrieben.

Für das lokale `infra-lab` zunächst die gesicherten Images importieren:

```sh
mise exec -- k3d image import -c infra-lab \
  panko-gateway:python-baseline-20261023 \
  panko-hook:python-baseline-20261023 \
  panko-runner:python-baseline-20261023
```

Die Komponenten lassen sich einzeln zurückstellen:

```sh
kubectl --kubeconfig .local/kubeconfig -n infra-gateway set image \
  deploy/platform-gateway gateway=panko-gateway:python-baseline-20261023
kubectl --kubeconfig .local/kubeconfig -n infra-gateway rollout status deploy/platform-gateway

kubectl --kubeconfig .local/kubeconfig -n infra-runners set image \
  deploy/database-hook hook=panko-hook:python-baseline-20261023
kubectl --kubeconfig .local/kubeconfig -n infra-runners rollout status deploy/database-hook

kubectl --kubeconfig .local/kubeconfig -n infra-runners set env \
  deploy/database-hook RUNNER_IMAGE=panko-runner:python-baseline-20261023
kubectl --kubeconfig .local/kubeconfig -n infra-runners rollout status deploy/database-hook
```

Eine Runner-Änderung betrifft nur neue Jobs. Laufende Jobs behalten ihr Image und
ihre Vorlage. Keine Jobs ersetzen und keine Finalizer umgehen, um einen Wechsel
zu erzwingen. Status, Logs und den normalen Lebenszyklus nach jedem Schritt prüfen.

Zurück zum Go-Standard: Go-Images mit `make build` importieren, den optionalen
Runner-Override mit `kubectl ... set env deploy/database-hook RUNNER_IMAGE-`
entfernen und `make deploy` ausführen. Für andere Cluster die Kubeconfig und den
Clusternamen explizit angeben.
