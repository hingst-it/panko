# PoC: Panko-Server vereinheitlichen und Installation vereinfachen

> **Status: abgeschlossener, überholter Auftrag.** Dieses Dokument ist die ursprüngliche
> Aufgabenbeschreibung des Proof of Concept (PR #70); die Auswertung steht in
> [docs/poc-unified-server.de.md](docs/poc-unified-server.de.md). Die Einschränkung „keine
> Veröffentlichung und kein Merge“ galt nur für den PoC. Der Release-Auftrag für v0.0.2 hat sie
> aufgehoben; das Ergebnis ist in v0.0.2 veröffentlicht. Aktuelles Verhalten beschreiben
> [docs/installation.md](docs/installation.md) und [docs/upgrading.md](docs/upgrading.md).

## Ziel und Rahmen

Den Ansatz als Proof of Concept in einem separaten Branch
`poc/unified-server-dev-install` umsetzen.

Der PoC soll die technische Machbarkeit und den vereinfachten Einstieg zeigen.
Keine Veröffentlichung und kein Merge nach `main` im Rahmen dieses Tasks.

Bestehende Installationen, Cluster und Daten erhalten. Für Tests ein eigenes,
disponibles Lab verwenden.

## Zielarchitektur

Zwei Programme bereitstellen:

- `panko`: Ressourcenverwaltung, Administration, Installation und Dev-Lab.
- `panko-server`: gemeinsame Binary für die Laufzeitrollen, einschließlich
  integriertem Metacontroller.

Kubernetes bleibt Betriebsgrundlage. Kein separates `panko-dev`.

### Server

Vorgesehenes Bedienbild:

```sh
panko-server gateway
panko-server controller
panko-server endpoint
panko-server mirror
panko-server runner apply ...
panko-server agent run ...
```

- Metacontroller als festgelegte Go-Abhängigkeit in die Controller-Rolle einbetten.
- Bestehenden Reconciliation-Hook zunächst über internes HTTP anbinden.
- Rollen in getrennten Pods oder Jobs mit eigenen ServiceAccounts starten.
- Gateway weiterhin ohne Secret-Zugriff betreiben.
- Teambezogene Runner-Rechte und State-Trennung erhalten.
- Start, Shutdown, Readiness, Logs und Metriken je Rolle berücksichtigen.
- Gemeinsames Server-Image verwenden; zusätzliche Images mit OpenTofu und Git
  sind bei Bedarf zulässig.
- OpenTofu und Git bleiben externe Werkzeuge.

Die Einbettbarkeit der gewählten Metacontroller-Version zuerst durch einen
kleinen Prototyp prüfen und Integrationsgrenzen dokumentieren.

## Helm-Chart und Installation

Ein versioniertes Panko-Helm-Chart erstellen, das folgende Bestandteile abdeckt:

- CRDs und explizites RBAC.
- Deployments, Services und Rollenkonfiguration.
- Integrierten Controller ohne separates Metacontroller-Deployment.
- Referenzen auf vorhandene Secrets.
- Optionale PostgreSQL- und S3-Dienste für das lokale Lab.
- Konfigurierbare Image-Repositories und festgelegte Image-Versionen.

Bestehende Admin-Kommandos unter `panko admin` integrieren.

Installationsbefehl ergänzen:

```sh
panko install --kubeconfig ./config --context mein-cluster
```

- Voraussetzungen und Berechtigungen prüfen.
- Zielcluster und Kontext eindeutig ausweisen.
- Dasselbe Chart wie für GitOps und das Dev-Lab verwenden.
- Installation beziehungsweise Aktualisierung durchführen und Readiness prüfen.
- Ohne Repository-Checkout, Make und lokale Image-Builds funktionieren.
- Zuständigkeiten zwischen Helm und CLI dokumentieren.
- CRD-Upgrades und den Erhalt bestehender Ressourcen ausdrücklich behandeln.

Für den PoC gebaute Images müssen im Testcluster verfügbar sein. Einen
reproduzierbaren Weg dafür dokumentieren; eine öffentliche Veröffentlichung
ist nicht erforderlich.

## GitOps: `helm template`, Argo CD und Flux

Das Chart muss unabhängig von `panko install` nutzbar sein. Die CLI darf keine
undokumentierten Voraussetzungen für eine funktionierende Installation schaffen.

Einen dokumentierten Rendering-Weg bereitstellen, beispielsweise:

```sh
helm template panko ./charts/panko \
  --namespace panko-system \
  --include-crds \
  --values examples/gitops/values.yaml \
  > rendered/panko.yaml
```

Chart-Pfade und Optionen an die tatsächliche Implementierung anpassen.

Dokumentieren und als Beispiele bereitstellen:

- Eine kommentierte `values.yaml` für einen vorhandenen Cluster.
- Rendering mit `helm template` und Prüfung der erzeugten Manifeste.
- Argo-CD-`Application` für die Installation aus dem Chart.
- Flux-Quelle und `HelmRelease` für denselben Installationsweg.
- Alternativ die Nutzung vorab gerenderter Manifeste.
- Benötigte Namespaces und vorhandene Secrets.
- Reihenfolge und Readiness-Abhängigkeiten bei CRDs und Custom Resources.
- Umgang mit Upgrades, CRDs, Pruning und persistenten Daten.
- Verfügbarkeit der PoC-Images und des Charts für den jeweiligen GitOps-Controller.

Generierte Secrets und zufällige Werte dürfen bei wiederholtem Rendering keinen
unbeabsichtigten Drift oder Credential-Wechsel verursachen. Für GitOps bevorzugt
vorhandene Secrets referenzieren; deren sichere Bereitstellung dokumentieren.

Die Beispiele dürfen keine echten Credentials enthalten. Keine Installation in
vorhandene Argo-CD- oder Flux-Umgebungen ohne gesonderten Auftrag durchführen.

## Lokales Lab mit `panko dev`

Vorgesehenes Bedienbild:

```sh
panko dev up
panko dev status
panko dev down
```

`dev up` soll:

1. Eine vorhandene, laufende Docker-Installation prüfen.
2. Eine unterstützte k3d-Version erkennen.
3. Fehlendes k3d nach ausdrücklicher Zustimmung privat installieren und dessen
   Prüfsumme verifizieren.
4. Einen dedizierten k3d-Cluster mit eigener Kubeconfig erstellen.
5. Dasselbe Helm-Chart mit Lab-Einstellungen installieren.
6. API und UI lokal erreichbar machen.
7. Bereitschaft prüfen und nächste Schritte ausgeben.

Zusätzlich:

- Wiederholtes `dev up` erhält ein kompatibles eigenes Lab.
- Abgebrochene Einrichtung lässt sich fortsetzen.
- Fremde Cluster und Namenskollisionen sicher behandeln.
- Hintergrundprozesse und deren Beendigung eindeutig verwalten.
- `dev status` gibt keine Secrets aus.
- `dev down` entfernt ausschließlich das zugeordnete Lab und verlangt eine
  ausdrückliche Bestätigung für dessen Datenverlust.
- Globale kubeconfig und Standardkontext bleiben unverändert.

Docker wird vorausgesetzt. Podman, gebündelte Container-Laufzeiten und
VM-Verwaltung sind außerhalb des PoC-Umfangs.

## Bestehende Eigenschaften bewahren

- Create, Update, Plan, explizites Retry und Destroy.
- Serialisierung pro Ressource und unveränderliche Ausführungssnapshots.
- Festgelegte Modulrevisionen.
- Finalizer bis zum erfolgreichen Destroy und erforderlichen Cleanup.
- Teambezogene State-Trennung und bereinigte Statusausgaben.
- Remote-Agenten und bestehende State-Backend-Funktionen.

## Akzeptanzkriterien

- Umsetzung vollständig auf dem separaten PoC-Branch.
- Gemeinsame Server-Binary startet alle vorgesehenen Rollen.
- Metacontroller läuft integriert in der Controller-Rolle.
- Rollen behalten getrennte ServiceAccounts und Berechtigungen.
- `panko install` funktioniert in einem dedizierten vorhandenen Testcluster.
- `panko dev up/status/down` funktioniert ohne Checkout beim Anwender.
- Wiederholter Start und Neustarts erhalten Ressourcen und State.
- Ein Ressourcenlebenszyklus einschließlich Fehler, Retry und Destroy ist geprüft.
- Helm-Lint und Rendering-Prüfungen bestehen.
- GitOps-Rendering ist reproduzierbar und benötigt keine verborgenen CLI-Schritte.
- Argo-CD- und Flux-Beispiele sind enthalten; der jeweilige Validierungsumfang
  ist dokumentiert.
- Fremde Cluster, Kontexte und Daten bleiben unverändert.
- Relevante Unit-, RBAC- und E2E-Prüfungen bestehen.

## Abschluss des PoC

Eine kurze Auswertung im Repository dokumentieren:

- Was funktioniert und wie es reproduziert wird.
- Bekannte Einschränkungen und offene Entscheidungen.
- Auswirkungen auf Berechtigungen, Abhängigkeiten und Updates.
- Migrationsbedarf für bestehende Installationen.
- Verbleibende Arbeit bis zu einer veröffentlichbaren Version.
