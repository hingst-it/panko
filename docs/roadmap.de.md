# Roadmap: Panko für den persönlichen Betrieb

Ziel: Eine Person betreibt Panko für sich selbst, etwa im Homelab oder für ein Nebenprojekt.
Die Roadmap entstand aus zwei unabhängigen Entwürfen (Claude, Codex) und einem gegenseitigen
Review. Mehrmandantenfähigkeit für fremde Teams und Unternehmensanforderungen sind kein Ziel.

## Meilenstein 1: erste nutzbare lokale Version

Erreicht, wenn eine frische Installation eine nützliche Ressource anlegen, einen Fehler
verständlich melden und gezielt wiederholen, Zugangsdaten bereitstellen, eine geprobte
Wiederherstellung überstehen und sauber löschen kann, jeweils über dokumentierte Abläufe.

| Nr. | Thema | Größe | Stand |
| --- | --- | --- | --- |
| 1 | Verständliche Fehler und explizites Wiederholen | L | umgesetzt |
| 1b | Pull-Through-Mirror für Provider | M | umgesetzt |
| 2 | Geprobte Sicherung und Wiederherstellung | M | umgesetzt, Probe-Wiederherstellung bestanden |
| 3 | Reproduzierbare persönliche Installation | M | umgesetzt |
| 4 | Alltagsbefehle (CLI) | M | umgesetzt |

### 1. Verständliche Fehler und explizites Wiederholen

Heute meldet ein Fehlschlag nur „Apply failed; inspect job logs" (`internal/hook/hook.go`);
über das Gateway sind keine Logs erreichbar, und Wiederholen erfordert eine Spec-Änderung.

- Jede Phase meldet einen festen Fehlercode mit sicherer Meldung und nächstem Schritt:
  Modulabruf (git-sync), Engine-Kopie, Backend-Initialisierung, Backend-Prüfung,
  Checkpoint, Provider-Ausführung, Aufräumen. Init-Container-Fehler gehören dazu.
- Ein begrenzter Ausführungsdatensatz im Status: Versuch, Job, Generation, Modulrevision,
  Zeitpunkte, Phase, Fehlercode, Änderungszahlen des Plans. Freitext aus OpenTofu wird
  nicht ungefiltert veröffentlicht.
- Apply und Destroy lassen sich über eng definierte Gateway-Aktionen wiederholen, ohne die
  Spec zu ändern und ohne beliebige Annotationen freizugeben.
- Fehlerdaten überleben die Job-TTL.

Abnahme: Ein gezielt herbeigeführter Fehler je Phase erscheint mit passendem Code im Status
und ist über das Gateway lesbar und wiederholbar.

### 1b. Pull-Through-Mirror für Provider

Jeder Job lädt Provider neu herunter; ein abgebrochener Download von GitHub hat im Lab
bereits einen Job scheitern lassen, auch Destroys sind betroffen.

- Kleiner Cache-Dienst im Cluster, der OpenTofus Network-Mirror-Protokoll spricht und bei
  Cache-Miss aus der Registry lädt.
- HTTPS-Mirror mit eigener CA; der Runner vertraut Systemwurzeln plus `/mirror-ca`
  über `SSL_CERT_DIR=/etc/ssl/certs:/mirror-ca`. Die CA wird als ConfigMap je Team verteilt.
- Vor `init` prüft der Runner `/healthz` mit zwei Sekunden Timeout, auch bei Migrationen.
  Erreichbar: CLI-Konfiguration mit `network_mirror`. Nicht erreichbar: direkter
  Registry-Zugriff und ein strukturiertes Warnereignis. OpenTofu verlangt HTTPS und
  fällt bei einem unerreichbaren konfigurierten Mirror nicht auf `direct` zurück.
  Fällt der Mirror erst nach der Probe aus, kann `init` deshalb scheitern.
- Indizes haben eine begrenzte TTL; Versionsmetadaten und ZIPs bleiben unverändert.
  ZIPs werden vor dem atomaren Speichern gegen die Registry-SHA-256 geprüft.
  Das Modul-Lockfile prüft die Provider zusätzlich im Runner.
- Nur Downloads laufen über den Mirror. Kein allgemeiner HTTPS-Forward-Proxy: Er könnte
  verschlüsselten Verkehr nicht cachen und würde Provider-, Backend- und API-Verbindungen
  beeinflussen. Die Runner-Umgebung entfernt `HTTP(S)_PROXY` weiterhin.

Abnahme: Zweiter Job lädt keine Provider mehr aus dem Internet; ein gestoppter Mirror
verhindert keinen Apply oder Destroy, wenn er bereits bei der Probe nicht erreichbar ist.

### 2. Geprobte Sicherung und Wiederherstellung

- Sicherung im Stillstand: CR-Identitäten und Bindungen, State, Checkpoints,
  Installations-ID, benötigte Secrets und die tatsächlichen Datenbankinhalte. Ein
  wiederhergestellter State allein stellt keine Daten wieder her.
- Lab-S3 liegt auf einem Cluster-Volume und ist damit nicht unabhängig dauerhaft.
- Wiederherstellung in ein getrenntes Lab als Probe; Checkpoint-Konflikte werden angezeigt,
  Entfernen bleibt eine bewusste Aktion.

Abnahme: Wiederherstellung in ein frisches Lab, danach Update und Destroy einer
wiederhergestellten Ressource ohne Neuanlage der Datenbank.

### 3. Reproduzierbare persönliche Installation

- Teams konfigurierbar statt fest `team-a`/`team-b` (`internal/admin/rbac.go`,
  `cluster/clean.sh`); ein Befehl legt ein Team samt Namespaces, RBAC, Token und
  S3-Zugang an.
- Eine Konfigurationsdatei für Teams, Modulrepository und -revision, Images und optionale
  Lab-Bestandteile; Lab-S3 optional, Kubernetes-State bleibt Standard.
- Dokumentierter Upgrade-Pfad. Helm-Chart und veröffentlichte Images erst bei Bedarf.

Abnahme: Installation mit nur einem selbst benannten Team aus der Konfigurationsdatei,
danach Upgrade auf eine neuere Revision ohne zusätzliche Applies.

### 4. Alltagsbefehle (CLI)

- Schlanke CLI über dem Gateway: anlegen, warten, anzeigen, wiederholen, verbinden, löschen.
- `doctor` prüft Konfiguration, RBAC, Modulzugriff und Backends.
- Zugangsdaten über einen bewussten lokalen Befehl, der das Connection-Secret liest; das
  Gateway erhält keinen allgemeinen Secret-Zugriff. Token-Rotation als Befehl.

Abnahme: Der komplette Lebenszyklus einer Datenbank inklusive Verbindungsaufbau ohne
kubectl und ohne curl.

## Danach

| Nr. | Thema | Größe | Anmerkung | Stand |
| --- | --- | --- | --- | --- |
| 3b | Modul-Mirror im Cluster | M | git-sync liest vom Mirror; der Deploy-Key liegt nur noch dort statt in jedem Team-Namespace | umgesetzt |
| 3c | Egress-Regeln für Runner | M | Erst nach 1b und 3b. Panko erzeugt NetworkPolicies aus deklarierten Zielen; Dienste werden auf ihre Pods aufgelöst, externe Ziele als CIDR und Port. Test muss auch Verweigerung zeigen | umgesetzt, optional (`"egress": true`) |
| 5 | Zweiter, echter Ressourcentyp | L | Mit eigenem Credential- und Output-Vertrag; der Runner lädt heute immer das PostgreSQL-Admin-Passwort | offen: erst mit einem tatsächlich benötigten Modul |
| 6 | Drift-Prüfung und kontrollierte Modul-Rollouts | L | Lesender Plan auf Abruf; Revisionswechsel pro Ressource statt flottenweit | umgesetzt |
| 7 | Minimale Oberfläche | M, Graph +M | Erst Inspektor (Ressourcen, Phase, letzte Ausführung, Fehlercode); Graph als zweiter Reiter aus `tofu graph`, als bereinigtes Artefakt ohne Attribute, Outputs und Instanzschlüssel, mit Team-Token und UID-Prüfung | umgesetzt (Inspektor und Graph) |

## Entscheidungen

- Egress-Regeln sind optional und standardmäßig aus; Downloads laufen über Mirrors.
- Fehlerrückmeldung vor Paketierung: Der Betreiber nutzt die bestehende Installation bereits.
- Ein zweiter Ressourcentyp erst mit einem tatsächlich benötigten Modul.
- Oberfläche erst, wenn Status und Wiederholen aussagekräftig sind; kein Graph-first.

## Bewusst nicht geplant

OIDC und Unternehmensberechtigungen, weitere Backend-Typen, von Teams mitgebrachter Code,
automatische Drift-Reparatur, ein mitgelieferter Monitoring-Stack, Ersatz von Metacontroller,
ein geteiltes Plugin-Cache-Verzeichnis für parallele Jobs.
