import Foundation
import Darwin

@main struct LocalMCPGrantProtocolTest {
    static func main() throws {
        let fixture = URL(fileURLWithPath: CommandLine.arguments[1])
        let grant = try JSONDecoder().decode(LocalMCPConnectionGrant.self, from: Data(contentsOf: fixture))
        precondition(grant.schemaVersion == 1 && grant.serverID == "example-server")
        precondition(grant.storeID == "example-store" && grant.daemonPID == 4242)
        let directory = FileManager.default.temporaryDirectory.resolvingSymlinksInPath()
            .appendingPathComponent("mcp-grant-test-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = try grant.save(in: directory)
        let decoded = try JSONDecoder().decode(LocalMCPConnectionGrant.self, from: Data(contentsOf: file))
        precondition(decoded.instanceID == grant.instanceID && decoded.accessToken == grant.accessToken)
        let attrs = try FileManager.default.attributesOfItem(atPath: file.path)
        precondition((attrs[.posixPermissions] as? NSNumber)?.intValue == 0o600)
        let dirAttrs = try FileManager.default.attributesOfItem(atPath: directory.path)
        precondition((dirAttrs[.posixPermissions] as? NSNumber)?.intValue == 0o700)
        do {
            try LocalMCPConnectionGrant.revoke(at: file, instanceID: "foreign")
            preconditionFailure("foreign instance revoked grant")
        } catch LocalMCPGrantError.unsafeFile {}
        precondition(FileManager.default.fileExists(atPath: file.path))
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        do {
            _ = try grant.save(in: directory)
            preconditionFailure("public existing file accepted")
        } catch LocalMCPGrantError.unsafeFile {}
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
        try LocalMCPConnectionGrant.revoke(at: file, instanceID: grant.instanceID)
        precondition(!FileManager.default.fileExists(atPath: file.path))
        let target = directory.appendingPathComponent("unrelated")
        try Data("user fixture".utf8).write(to: target)
        try FileManager.default.createSymbolicLink(at: file, withDestinationURL: target)
        do {
            _ = try grant.save(in: directory)
            preconditionFailure("symlink accepted")
        } catch LocalMCPGrantError.unsafeFile {}
        let remaining = try String(contentsOf: target, encoding: .utf8)
        precondition(remaining == "user fixture")
        print("Local MCP Go/Swift fixture, private write, revoke, and symlink checks passed")
    }
}
