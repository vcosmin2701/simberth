import AppKit
import Foundation

/// Streams live simulator screens into the Runs view.
///
/// `simberth mirror` writes length-prefixed JPEG frames for every simulator on
/// one pipe. This reads them and hands back the newest frame per simulator;
/// frames are replaced rather than queued, since a stale screen is worthless
/// and buffering them would grow without bound.
@MainActor
final class MirrorSession: ObservableObject {
  /// One session for the app. `init` is failable (the CLI may be missing), and
  /// a failable initializer cannot back a `@StateObject`, so views share this.
  static let shared = MirrorSession()

  /// Newest frame per simulator UDID.
  @Published private(set) var screens: [String: NSImage] = [:]

  /// Not main-actor isolated so `deinit` can terminate it without hopping actors.
  private nonisolated(unsafe) var process: Process?
  private let executableURL: URL?

  init(bundle: Bundle = .main) {
    if let override = ProcessInfo.processInfo.environment["SIMBERTH_CLI"], !override.isEmpty {
      executableURL = URL(fileURLWithPath: override)
    } else {
      executableURL = bundle.url(forResource: "simberth", withExtension: nil)
    }
  }

  /// Mirroring is best-effort: without a CLI the run still works, it just has
  /// no live screens, so this never fails the run itself.
  private var isAvailable: Bool {
    guard let executableURL else { return false }
    return FileManager.default.isExecutableFile(atPath: executableURL.path)
  }

  /// Starts mirroring the given simulators, replacing any current session.
  func start(udids: [String], fps: Int = 5, scale: Double = 0.4) {
    stop()
    guard !udids.isEmpty, isAvailable, let executableURL else { return }

    let process = Process()
    process.executableURL = executableURL
    process.arguments =
      ["mirror", "--fps", String(fps), "--scale", String(scale)] + udids
    process.standardInput = FileHandle.nullDevice
    process.standardError = FileHandle.nullDevice

    let pipe = Pipe()
    process.standardOutput = pipe
    self.process = process

    // Parsing happens off the main actor; only the decoded image hops back.
    let handle = pipe.fileHandleForReading
    Task.detached(priority: .utility) { [weak self] in
      var buffer = Data()
      while true {
        let chunk = handle.availableData
        if chunk.isEmpty { break }
        buffer.append(chunk)

        while let (udid, jpeg, consumed) = MirrorSession.nextFrame(in: buffer) {
          buffer.removeSubrange(buffer.startIndex..<consumed)
          // Decoding stays off the main actor; only the finished image hops.
          guard let image = NSImage(data: jpeg) else { continue }
          await self?.publish(udid: udid, image: image)
        }

        // A runaway producer must not grow the buffer without bound.
        if buffer.count > 8 * 1024 * 1024 { buffer.removeAll(keepingCapacity: true) }
      }
    }

    try? process.run()
  }

  /// Publishes a decoded frame on the main actor.
  private func publish(udid: String, image: NSImage) {
    screens[udid] = image
  }

  func stop() {
    if let process, process.isRunning { process.terminate() }
    process = nil
    screens.removeAll()
  }

  deinit {
    // A mirror must never outlive its view; an orphaned process would keep
    // capturing frames from simulators nothing is watching.
    if let process, process.isRunning { process.terminate() }
  }

  /// Parses one `--simberth-frame` record, returning its payload and the index
  /// just past it, or nil when the buffer holds no complete frame yet.
  private nonisolated static func nextFrame(in buffer: Data) -> (String, Data, Data.Index)? {
    let separator = Data("\r\n\r\n".utf8)
    guard let headerEnd = buffer.range(of: separator) else { return nil }

    let header = String(decoding: buffer[buffer.startIndex..<headerEnd.lowerBound], as: UTF8.self)
    var udid = ""
    var length = 0
    for line in header.split(separator: "\r\n", omittingEmptySubsequences: true) {
      let parts = line.split(separator: ":", maxSplits: 1)
      guard parts.count == 2 else { continue }
      let value = parts[1].trimmingCharacters(in: .whitespaces)
      switch parts[0].lowercased() {
      case "udid": udid = value
      case "length": length = Int(value) ?? 0
      default: break
      }
    }
    guard length > 0, !udid.isEmpty else { return nil }

    let start = headerEnd.upperBound
    guard let end = buffer.index(start, offsetBy: length, limitedBy: buffer.endIndex),
      buffer.distance(from: start, to: buffer.endIndex) >= length
    else { return nil }

    return (udid, Data(buffer[start..<end]), end)
  }
}
