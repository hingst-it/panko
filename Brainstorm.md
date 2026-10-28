# Brainstorm: Panko als schlanke Service-Plattform

Stand: Diskussion vom 28. Oktober 2026. Dieses Dokument hält Ziele, Vorschläge
und offene Designentscheidungen fest; es ist eine **Roadmap-Sammlung, keine
Beschreibung des implementierten Verhaltens**. Der konkrete Installations-PoC steht
in [`POC.md`](POC.md).

> **Abgleich mit v0.0.2.** Umgesetzt sind aus diesem Dokument: `panko` mit
> Administration, Installation und Dev-Lab; `panko-server` mit eingebettetem
> Metacontroller und getrennten Rollen; ein Helm-Chart für `panko install`, `panko dev`,
> Helm, Argo CD und Flux; die Kategorie `panko` und der Verzicht auf Shortnames für
> Panko-CRDs ([docs/naming.md](docs/naming.md)). **Nicht umgesetzt** und weiterhin
> Vorschlag sind: API-Gruppen `<anbieterteam>.panko.dev` und die Kategorie
> `panko-<anbieterteam>`, Vertragsversionen und deren Bindung an Modulrevisionen,
> `panko upgrade`, Plan- und Graph-Artefakte im Status, Secret-Anbindung pro Team,
> kurzlebige Secrets für Remote-Agents und eine Durchsetzung per Kyverno. Die offene
> Entscheidung zu Vertragsversion und Implementierungsrevision bleibt offen.

## Ziel und Mehrwert

Das Plattformteam veröffentlicht einen Service; der Kunde bestellt ihn.
Panko soll CI/CD und Betrieb für das Plattformteam sowie die Bestellung für
Kunden vereinfachen. Vorhandene OpenTofu-Module bleiben die Implementierung.

Runner, Agents, State-Verwaltung und Ausführungssteuerung gehören zum Produkt.
Ihr Vorhandensein soll im Alltag kaum zusätzlichen Aufwand verursachen.
„Schlank“ bedeutet geringe Lern- und Betriebslast für Anwender, nicht bloß eine
kleine Binary oder möglichst wenige interne Komponenten.

Plattformteams liefern Vertrag, Modul und freigegebene Berechtigungen. Kunden
sehen fachliche Eingaben, Status und den Zugang zum bestellten Dienst, keine
Modulquellen, Provider-Credentials, State-Konfiguration oder Runner-Auswahl.

## Lieferung und Installation

- `panko` bündelt Ressourcenverwaltung, Administration, Installation und Dev-Lab.
- `panko-server` bündelt Laufzeitrollen einschließlich Metacontroller.
- Rollen laufen weiterhin separat mit eigenen ServiceAccounts und RBAC.
- Gemeinsame Binary bedeutet keine Zusammenlegung aller Rechte in einen Prozess.
- `panko install` installiert per Helm in einen explizit gewählten Cluster.
- `panko dev up/status/down` verwaltet ein dediziertes lokales k3d-Lab.
- Docker wird vorausgesetzt; keine eigene Laufzeit oder VM mitliefern.
- k3d kann nach Zustimmung versioniert und mit Prüfsummenprüfung privat
  heruntergeladen werden. Keine separate `panko-dev`-Variante nötig.
- Helm-Chart auch unabhängig von der CLI über `helm template`, Argo CD und Flux
  verwenden können. Keine versteckten Initialisierungsschritte in der CLI.
- Ein vorhandener Kubernetes-Cluster bleibt Voraussetzung für den Serverbetrieb.
- Docker Compose ersetzt die benötigten Kubernetes-Funktionen nicht.

## Plattformvertrag und CI/CD

Angestrebter Ablauf:

1. Plattformteam entwickelt Modul und Ressourcenvertrag in seinem Repository.
2. Wiederverwendbare CI prüft Vertrag, Modul, Kompatibilität und Lebenszyklus.
3. Release veröffentlicht eine nachvollziehbare, unveränderliche Service-Revision.
4. Argo CD oder Flux verteilt die freigegebene Definition.
5. Panko stellt den Service zur Bestellung bereit und steuert Ausführungen.

Kein eigener Controller und keine individuelle Ausführungspipeline pro Modul.
Felder sollen nicht mehrfach zwischen CRD, API und Modul zugeordnet werden müssen.
Verträge müssen neben Eingaben auch Status, Fehler, Löschverhalten und
Upgrade-Regeln verständlich festlegen.

## CRD-Namen und Discovery

Vorgeschlagene Konvention:

```yaml
metadata:
  name: databases.data-platform.panko.dev
spec:
  group: data-platform.panko.dev
  scope: Namespaced
  names:
    kind: Database
    singular: database
    plural: databases
    categories:
      - panko
      - panko-data-platform
    # Keine shortNames
```

```yaml
apiVersion: data-platform.panko.dev/v1alpha1
kind: Database
metadata:
  name: orders
  namespace: shop-team
spec:
  size: small
```

- Gruppenname `${anbieterteam}.panko.dev`: das Team, das den Typ verantwortet,
  nicht automatisch jedes konsumierende Team.
- Konsumenten über Namespaces und Berechtigungen trennen.
- Versionen im Kubernetes-Stil: `v1alpha1`, `v1beta1`, `v1`.
- Versionierung gilt pro CRD, nicht global oder pro Team.
- Kategorien automatisch `panko` und `panko-${anbieterteam}` setzen.
- Shortnames für Panko-CRDs nicht erlauben.
- Keine Shortnames zu vergeben verhindert nicht die Mehrdeutigkeit identischer
  Pluralnamen in verschiedenen API-Gruppen. Vollqualifizierte Namen in
  generierten Befehlen und Dokumentation verwenden.
- Discovery, API-Routing und Ressourcenidentitäten müssen API-Gruppen
  unterscheiden; Version plus Plural allein reicht nicht.
- Kategorien sind Discovery-Hilfen, keine Sicherheitsgrenzen.
- Diese Regeln könnten durch Kyverno für Panko-CRDs durchgesetzt werden.

## Vertragsversion, Modulbindung und Upgrades

Diskutierte strenge Vertragsregel:

> Jede funktionale Änderung am veröffentlichten Spec-Vertrag braucht eine neue
> API-Version derselben CRD. Beschreibungsänderungen brauchen keine neue Version.

Das würde auch optionale Feld-Ergänzungen einschließen und ist bewusst strenger
als kompatible Erweiterungen innerhalb einer Kubernetes-API-Version.
Typen, Pflichtfelder, Defaults, Validierungen und Semantik gehören zum Vertrag.

Modulquelle und Modulrevision sollen nicht durch Besteller variierbar sein.
Das Plattformteam bestimmt Repository, vollständigen Commit und Modulpfad.
Die ausgeführte Revision wird nachvollziehbar und dauerhaft gebunden.

Offene Entscheidung: Zunächst wurde eine unveränderliche Zuordnung jeder
CRD-Version zu genau einer Modulrevision diskutiert. Später wurde als Alternative
empfohlen, Vertragsversion und Implementierungsrelease getrennt zu behandeln,
damit interne Bugfixes nicht zwangsläufig Kunden-Upgrades benötigen. Diese
Alternative wurde noch nicht abschließend entschieden. Keine der beiden
Varianten stillschweigend als beschlossen behandeln.

Kubernetes-API-Versionen sind Darstellungen derselben Ressource, keine getrennten
Bestände und kein zuverlässiger Speicher für die ausgewählte Implementierung.
Lesen oder Konvertieren als neue Version darf keinen Infrastrukturwechsel auslösen.

Upgrades serverseitig koordinieren. Die CLI bietet lediglich den Einstieg:

```sh
panko upgrade orders --to-version v1alpha2
```

Das ist ein Bedienvorschlag, kein bestehender Befehl. Zielmodell, Freigabe,
Konvertierung, State-Migration und Verhalten nach einem teilweise erfolgreichen
Apply sind noch festzulegen. CLI, UI, API und GitOps müssen dieselben Regeln nutzen.

## Plan und Graph als Ausführungsergebnisse

Panko besitzt bereits Plan-/Drift-Funktionen und einen bereinigten Ressourcengraphen.
Darauf aufbauen statt parallele Mechanismen einzuführen.

- Kurze Zusammenfassungen und Artefaktreferenzen unter `status` vorsehen.
- Vollständige Graphen und JSON-Pläne nicht direkt in Custom Resources einbetten.
- Artefakte an konkrete Ausführung, Ressourcen-UID, Spec-Stand, Modulrevision und
  State-Basis binden. Änderungen können einen freigegebenen Plan ungültig machen.
- Geplanten Graph und Graph des zuletzt erfolgreichen Apply unterscheiden.
- JSON-Plan dient Review und Policy-Auswertung. Der ausführbare gespeicherte Plan
  ist ein anderes Artefakt; Freigabe und Apply müssen dieselbe Ausführung meinen.
- JSON-Pläne können Konfiguration, bisherigen State und Credentials enthalten.
  Geschützte Speicherung und gezielt bereinigte Ansichten sind erforderlich.

Beispiel eines noch nicht beschlossenen Statusmodells:

```yaml
status:
  lastExecution:
    id: run-123
    observedGeneration: 4
    moduleRevision: "<vollständiger Commit>"
    plan:
      summary:
        create: 1
        update: 2
        replace: 0
        delete: 0
      artifactRef: plan-run-123
    graph:
      artifactRef: graph-run-123
```

## Secrets einmal pro Team anbinden

Ziel: Einen Secretstore pro Team an Kubernetes anbinden und freigegebene Services
danach über Referenzen mit Credentials versorgen. Panko soll nicht für jeden
Secretstore einen eigenen Adapter entwickeln müssen.

- Plattformbetrieb stellt eine Integration bereit, beispielsweise External Secrets.
- Team konfiguriert Store-Zugang und zulässige Berechtigungen.
- Service-Definition deklariert freigegebene Credential-Referenzen.
- Besteller dürfen diese nicht durch beliebige Secret-Namen ersetzen.
- Nur berechtigte Ausführungen erhalten benötigte Werte; Gateway bleibt ohne
  pauschalen Zugriff auf Secrets.
- Kubernetes-Anbindung allein löst den Zugriff außerhalb des Clusters nicht.

## Kurzlebiger Secret-Zugriff für Remote-Agents

Terraform und OpenTofu bieten `ephemeral`-Ressourcen und -Variablen. Ephemeral
Werte werden nicht in Plan oder State gespeichert. Unterstützte Provider können
Secrets temporär beziehen; normale Data Sources sind kein gleichwertiger Ersatz.

- Terraform: Ephemeral-Ressourcen seit 1.10.
- OpenTofu: Ephemeral-Ressourcen seit 1.11.
- Verwendung unter anderem in Provider-Konfiguration oder unterstützten
  Write-only-Attributen; nicht beliebig in normalen Ressourcenattributen.
- `sensitive = true` allein verhindert keine Speicherung in State oder Plan.

Zwei mögliche Integrationswege:

1. Agent erhält eine kurzlebige Secretstore-Identität und nutzt vorhandene
   Ephemeral-Provider direkt. Erfordert Store-Zugang und Integration am Agent.
2. Panko vermittelt freigegebene Werte für einen autorisierten Run. Das Modul
   erhält sie als ephemeral Variablen. Passt zur zentralen Kubernetes-Anbindung
   und braucht zunächst keinen eigenen Panko-Terraform-Provider.

Den zweiten Weg zuerst auf Machbarkeit prüfen. Noch kein beschlossenes Protokoll.
Autorisierung muss an Team, Run, Agent, freigegebene Referenzen und Gültigkeitsdauer
gebunden sein. Plan und Apply sind getrennte Operationen und benötigen gegebenenfalls
jeweils neue Berechtigungen beziehungsweise Werte.

Ephemerality schützt nicht vor Kopieren oder Ausleiten durch den ausführenden
Prozess. Temporär ausgegebene statische Passwörter bleiben gültig. Echte zeitliche
Begrenzung braucht dynamische Credentials, TTL, Erneuerung und Widerruf.
Bei Agent-Ausfall darf Cleanup nicht allein vom Agent abhängen.

## Leitplanken für weitere Entscheidungen

- Bestehende Module einfach als fachlichen Service veröffentlichen können.
- Sichere Defaults und gemeinsame Releases statt manuell verdrahteter Komponenten.
- Explizite Retries, Finalizer, Team-Trennung und bereinigte Statusausgaben erhalten.
- Vorhandene Cluster, Daten, Kontexte und States bewahren.
- Keine Credentials in Git, Chat, Logs oder öffentlichen Artefakten.
- Vertragsabstraktion, Betriebsvereinfachung und Fehlersuche mit fremden Anwendern
  prüfen; geringe interne Komponentenanzahl ist allein kein Erfolgsmaßstab.
- Keine allgemeine Workflow- oder Policy-Plattform bauen, bevor ein konkreter
  Service-Lebenszyklus deren Bedarf zeigt.

## Referenzen

- [Kubernetes CRDs](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/)
- [CRD-Versionierung](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/)
- [Terraform ephemeral](https://developer.hashicorp.com/terraform/language/block/ephemeral)
- [OpenTofu ephemeral](https://opentofu.org/docs/language/ephemerality/ephemeral-resources/)
- [OpenTofu ephemeral Variablen](https://opentofu.org/docs/language/values/variables/)
- [OpenTofu JSON-Plan und State](https://opentofu.org/docs/v1.10/cli/commands/show/)
- [Vault Ephemeral-Provider](https://registry.terraform.io/providers/hashicorp/vault/latest/docs/guides/using_ephemeral_resources)
