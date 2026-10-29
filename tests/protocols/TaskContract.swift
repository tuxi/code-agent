// Standalone wire contract check. Foundation only; no AgentKit UI changes.
// Compile/run instructions are in docs/p0b1-implementation.md.
import Foundation

indirect enum JSONValue: Codable {
    case null, bool(Bool), number(Double), string(String), array([JSONValue]), object([String: JSONValue])
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode(Double.self) { self = .number(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) }
        else { self = .object(try c.decode([String: JSONValue].self)) }
    }
    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .string(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        }
    }
}
struct SessionRef: Codable {
    let kind: String
    let serverId, storeId, sessionId, agentId, runtimeKind, sessionHandle, nativeSessionId: String?
}
struct SourceRef: Codable {
    let kind, serverId, storeId, sessionId, turnId, requestId, seq, producerSubmissionId: String?
}
struct Participation: Codable {
    let participationId, taskId, agentId, role, observationLevel: String
    let sessionRef: SessionRef
    let capabilities: [String]
}
struct Attempt: Codable {
    let attemptId, taskId, participationId, stage, status: String
    let iteration: Int
    let sourceRef: SourceRef
    let executionEvidence: JSONValue?
}
struct ArtifactRef: Codable {
    let artifactId, attemptId, kind, version, captureMode: String
    let contentHash: String?
    let sourceRef: SourceRef
    let truncated: Bool
    let content: JSONValue?
}
struct Snapshot: Codable {
    let schema, taskId, workspaceRef, goal, mode, state, phase, revision, snapshotTaskSeq, taskAcceptance: String
    let waitReason, currentAttemptId, workflowRef: String?
    let acceptance, availableActions: [String]
    let participations: [Participation]
    let attempts: [Attempt]
    let artifactRefs: [ArtifactRef]
    let pendingAttention: [JSONValue]?
    let usage: JSONValue?
    let costUsd: Double?
}
struct TaskEvent: Codable {
    let schemaVersion: Int
    let eventId, taskId, taskRevision, recordedAt: String
    var kind, taskSeq: String
    let participationId, attemptId: String?
    let source: SourceRef
    let payload: JSONValue
}
struct EventPage: Codable {
    let schema, taskId, after, nextCursor: String
    let events: [TaskEvent]
    let hasMore, resyncRequired: Bool
}
struct Fixture: Decodable { let snapshot: Snapshot; let eventPage: EventPage }

guard CommandLine.arguments.count == 2 else { fatalError("provide the shared JSON fixture path") }
let decoder = JSONDecoder()
decoder.keyDecodingStrategy = .convertFromSnakeCase
let fixture = try decoder.decode(Fixture.self, from: Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1])))
let s = fixture.snapshot
precondition(s.schema == "task-snapshot/v1" && fixture.eventPage.schema == "task-event-page/v1")
precondition(s.taskId == fixture.eventPage.taskId && s.snapshotTaskSeq == fixture.eventPage.nextCursor)
precondition(s.costUsd == nil && s.usage == nil && s.workflowRef == nil && s.taskAcceptance == "not_reviewed")
let parts = Dictionary(uniqueKeysWithValues: s.participations.map { ($0.participationId, $0) })
precondition(parts["part_plan"]!.sessionRef.sessionHandle == parts["part_review"]!.sessionRef.sessionHandle)
precondition(parts["part_plan"]!.role != parts["part_review"]!.role)
for part in s.participations {
    precondition(part.taskId == s.taskId)
    if part.observationLevel == "reported" { precondition(!part.capabilities.contains("link_attempt")) }
}
let attempts = Dictionary(uniqueKeysWithValues: s.attempts.map { ($0.attemptId, $0) })
for attempt in s.attempts { precondition(parts[attempt.participationId] != nil) }
for artifact in s.artifactRefs { precondition(attempts[artifact.attemptId] != nil && artifact.captureMode != "immutable_snapshot") }
var unknown = fixture.eventPage.events[0]
unknown.kind = "future.progress"
unknown.taskSeq = "9007199254740993"
let encoder = JSONEncoder()
encoder.keyEncodingStrategy = .convertToSnakeCase
let roundTrip = try decoder.decode(TaskEvent.self, from: encoder.encode(unknown))
precondition(roundTrip.kind == unknown.kind && roundTrip.taskSeq == unknown.taskSeq)
print("Go/Swift shared B1 fixture: PASS")
