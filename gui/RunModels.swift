import Foundation

/// Top-level sections of the app. The existing `SlimmingMode` picker stays what
/// it always was — a choice *within* the simulator section — so adding Runs here
/// leaves every `slimmingMode == .memory` branch untouched.
enum AppSection: String, CaseIterable, Identifiable {
  case simulators = "Simulators"
  case runs = "Runs"

  var id: String { rawValue }

  var systemImage: String {
    switch self {
    case .simulators: return "iphone"
    case .runs: return "play.circle"
    }
  }
}

/// Mirrors `RunStatus` in run.go. Decoded from the event stream, so the raw
/// values must match the Go constants exactly.
enum RunStatus: String, Decodable, Equatable {
  case running
  case passed
  case failed
  case error
  case skipped

  var title: String {
    switch self {
    case .running: return "Running"
    case .passed: return "Passed"
    case .failed: return "Failed"
    case .error: return "Error"
    case .skipped: return "Skipped"
    }
  }

  var systemImage: String {
    switch self {
    case .running: return "circle.dotted"
    case .passed: return "checkmark.circle.fill"
    case .failed: return "xmark.circle.fill"
    case .error: return "exclamationmark.triangle.fill"
    case .skipped: return "minus.circle"
    }
  }
}

/// One event from `simberth run --json`. Mirrors the `Event` struct in run.go;
/// the Go side emits camelCase matching these property names, and the decoder
/// sets no key strategy, so the spellings are the contract.
struct RunEvent: Decodable {
  let type: String
  let at: Int
  let runId: String?
  let udid: String?
  let simName: String?
  let stepId: Int?
  let tool: String?
  let summary: String?
  let screenshot: String?
  let durationMs: Int?
  let status: RunStatus?
  let detail: String?
  let scenario: String?
  let appPath: String?
  let simCount: Int?
}

/// One tool call an agent made.
struct RunStep: Identifiable, Equatable {
  let id: Int
  var tool: String
  var summary: String
  var status: RunStatus
  var durationMs: Int
  var screenshot: String?
  var detail: String?

  var durationText: String {
    durationMs < 1000 ? "\(durationMs)ms" : String(format: "%.1fs", Double(durationMs) / 1000)
  }
}

/// One simulator's slice of a run.
struct SimRun: Identifiable, Equatable {
  let udid: String
  var simName: String
  var status: RunStatus
  var steps: [RunStep]
  var detail: String?
  /// The agent's most recent reasoning, shown while a run is in flight.
  var thinking: String?

  var id: String { udid }

  var passedSteps: Int { steps.filter { $0.status == .passed }.count }

  /// The step the agent is on right now, for the card's subtitle.
  var currentStep: RunStep? {
    steps.last { $0.status == .running } ?? steps.last
  }
}

/// A scenario executed across a fleet.
struct LiveRun: Equatable {
  var id: String = ""
  var scenario: String = ""
  var appPath: String?
  var status: RunStatus = .running
  var startedAt: Int = 0
  var endedAt: Int?
  var sims: [SimRun] = []

  var isFinished: Bool { status != .running }

  var elapsedText: String {
    let end = endedAt ?? Int(Date().timeIntervalSince1970 * 1000)
    let seconds = max(0, (end - startedAt) / 1000)
    return seconds < 60 ? "\(seconds)s" : "\(seconds / 60)m \(seconds % 60)s"
  }

  /// Folds one event into the run. This is the Swift counterpart of `Run.Apply`
  /// in run.go: the GUI builds its state the same way the CLI does, by
  /// replaying the stream, so both agree on what a run looks like.
  mutating func apply(_ event: RunEvent) {
    switch event.type {
    case "run.started":
      id = event.runId ?? id
      scenario = event.scenario ?? scenario
      appPath = event.appPath
      startedAt = event.at
      status = .running
      return
    case "run.finished":
      status = event.status ?? .error
      endedAt = event.at
      return
    default:
      break
    }

    guard let udid = event.udid else { return }
    let index = simIndex(for: udid, name: event.simName)

    switch event.type {
    case "sim.leased", "sim.ready", "agent.started":
      sims[index].status = .running
    case "agent.thinking":
      sims[index].thinking = event.detail
    case "step.started":
      sims[index].steps.append(
        RunStep(
          id: event.stepId ?? sims[index].steps.count + 1,
          tool: event.tool ?? "",
          summary: event.summary ?? event.tool ?? "",
          status: .running,
          durationMs: 0
        ))
    case "step.finished":
      // Matched by id rather than position: an agent can have several tool
      // calls in flight within one turn.
      if let stepIndex = sims[index].steps.firstIndex(where: { $0.id == event.stepId }) {
        sims[index].steps[stepIndex].status = event.status ?? .passed
        sims[index].steps[stepIndex].durationMs = event.durationMs ?? 0
        sims[index].steps[stepIndex].screenshot = event.screenshot
        sims[index].steps[stepIndex].detail = event.detail
      }
    case "agent.finished":
      sims[index].status = event.status ?? .error
      sims[index].detail = event.detail
    case "error":
      sims[index].status = .error
      sims[index].detail = event.detail
    default:
      break  // unknown events are ignored so an older app keeps working
    }
  }

  private mutating func simIndex(for udid: String, name: String?) -> Int {
    if let existing = sims.firstIndex(where: { $0.udid == udid }) {
      if let name, !name.isEmpty { sims[existing].simName = name }
      return existing
    }
    sims.append(
      SimRun(udid: udid, simName: name ?? udid, status: .running, steps: [], detail: nil))
    return sims.count - 1
  }
}

/// A scenario the user can launch from the app.
struct ScenarioDraft: Equatable {
  var text: String = ""
  var appPath: String = ""
  var simCount: Int = 2
  var slim: Bool = true

  /// Seeds the form from the environment, so a demo or a UI test can launch the
  /// app with a scenario already filled in.
  static func fromEnvironment() -> ScenarioDraft {
    var draft = ScenarioDraft()
    let env = ProcessInfo.processInfo.environment
    if let text = env["SIMBERTH_SCENARIO"] { draft.text = text }
    if let app = env["SIMBERTH_APP"] { draft.appPath = app }
    if let count = env["SIMBERTH_SIMS"], let value = Int(count) { draft.simCount = value }
    if let slim = env["SIMBERTH_SLIM"] { draft.slim = slim != "0" && slim != "false" }
    return draft
  }

  var isRunnable: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
}
