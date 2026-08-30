use crate::agg;
use crate::data::{self, AgentKind, LoadedData};
use chrono::{Local, NaiveDate, TimeDelta, TimeZone};
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::sync::Arc;

#[derive(Clone, Copy, PartialEq, Eq)]
enum OutputFormat {
    Table,
    Csv,
}

struct Options {
    agent: Option<AgentKind>,
    since: Option<NaiveDate>,
    until: Option<NaiveDate>,
    days: i64,
    format: OutputFormat,
    refresh: bool,
    by_agent: bool,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            agent: Some(AgentKind::Claude),
            since: None,
            until: None,
            days: 30,
            format: OutputFormat::Table,
            refresh: false,
            by_agent: false,
        }
    }
}

#[derive(Default)]
struct DailyUsage {
    models: BTreeSet<String>,
    input: f64,
    output: f64,
    cache_creation: f64,
    cache_read: f64,
    cost: f64,
    unpriced_models: BTreeSet<String>,
}

impl DailyUsage {
    fn total_tokens(&self) -> f64 {
        self.input + self.output + self.cache_creation + self.cache_read
    }

    fn merge(&mut self, other: &Self) {
        self.models.extend(other.models.iter().cloned());
        self.input += other.input;
        self.output += other.output;
        self.cache_creation += other.cache_creation;
        self.cache_read += other.cache_read;
        self.cost += other.cost;
        self.unpriced_models
            .extend(other.unpriced_models.iter().cloned());
    }
}

pub fn run(args: Vec<String>) -> Result<(), String> {
    let Some(options) = parse_options(&args)? else {
        print_help();
        return Ok(());
    };
    let today = Local::now().date_naive();
    let until = options.until.unwrap_or(today);
    let since = options
        .since
        .unwrap_or_else(|| until - TimeDelta::days(options.days - 1));
    if until < since {
        return Err(format!("--until {until} 不能早于 --since {since}"));
    }

    let start = local_day_start(since)?;
    let end = local_day_start(
        until
            .succ_opt()
            .ok_or_else(|| format!("无法计算 {until} 的下一天"))?,
    )?;
    let loaded = match (options.agent, options.refresh) {
        (Some(agent), true) => {
            data::reload_agent_from(Arc::new(LoadedData::default()), start, end, agent)
        }
        (Some(agent), false) => data::load_agent(agent, start, end),
        (None, true) => data::reload_all(start, end),
        (None, false) => data::load_all(start, end),
    };
    let rows = summarize(&loaded, options.agent, since, until);
    let by_agent_rows = options.by_agent.then(|| {
        AgentKind::ALL
            .into_iter()
            .filter_map(|agent| {
                let rows = summarize(&loaded, Some(agent), since, until);
                (!rows.is_empty()).then_some((agent, rows))
            })
            .collect::<Vec<_>>()
    });
    match (options.format, by_agent_rows.as_deref()) {
        (OutputFormat::Table, Some(agent_rows)) => print_by_agent_table(&rows, agent_rows),
        (OutputFormat::Csv, Some(agent_rows)) => print_by_agent_csv(&rows, agent_rows),
        (OutputFormat::Table, None) => print_table(&rows),
        (OutputFormat::Csv, None) => print_csv(&rows),
    }

    let unpriced: BTreeSet<&str> = rows
        .values()
        .flat_map(|row| row.unpriced_models.iter().map(String::as_str))
        .collect();
    if !unpriced.is_empty() {
        eprintln!(
            "警告：以下模型未找到定价，Cost 未包含它们：{}",
            unpriced.into_iter().collect::<Vec<_>>().join(", ")
        );
    }
    for error in loaded.errors {
        eprintln!("数据源警告：{}", error.message);
    }
    Ok(())
}

fn parse_options(args: &[String]) -> Result<Option<Options>, String> {
    if args.iter().any(|arg| arg == "--help" || arg == "-h") {
        return Ok(None);
    }
    let mut options = Options::default();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--agent" => {
                i += 1;
                let value = args.get(i).ok_or("--agent 缺少值")?;
                options.agent = parse_agent(value)?;
            }
            "--since" => {
                i += 1;
                let value = args.get(i).ok_or("--since 缺少值")?;
                options.since = Some(parse_date(value)?);
            }
            "--until" => {
                i += 1;
                let value = args.get(i).ok_or("--until 缺少值")?;
                options.until = Some(parse_date(value)?);
            }
            "--days" => {
                i += 1;
                let value = args.get(i).ok_or("--days 缺少值")?;
                options.days = value
                    .parse::<i64>()
                    .map_err(|_| format!("无效的 --days：{value}"))?;
                if options.days <= 0 {
                    return Err("--days 必须大于 0".into());
                }
            }
            "--format" => {
                i += 1;
                options.format = match args.get(i).map(String::as_str) {
                    Some("table") => OutputFormat::Table,
                    Some("csv") => OutputFormat::Csv,
                    Some(value) => return Err(format!("不支持的格式：{value}")),
                    None => return Err("--format 缺少值".into()),
                };
            }
            "--refresh" => options.refresh = true,
            "--by-agent" => options.by_agent = true,
            value => return Err(format!("未知参数：{value}")),
        }
        i += 1;
    }
    Ok(Some(options))
}

fn parse_agent(value: &str) -> Result<Option<AgentKind>, String> {
    match value.to_ascii_lowercase().replace('_', "-").as_str() {
        "all" => Ok(None),
        "devin" => Ok(Some(AgentKind::Devin)),
        "amp" => Ok(Some(AgentKind::Amp)),
        "claude" | "claude-code" => Ok(Some(AgentKind::Claude)),
        "codex" => Ok(Some(AgentKind::Codex)),
        "antigravity" => Ok(Some(AgentKind::Antigravity)),
        "grok" | "grok-build" => Ok(Some(AgentKind::Grok)),
        "zcode" | "z-code" => Ok(Some(AgentKind::ZCode)),
        "opencode" | "open-code" => Ok(Some(AgentKind::OpenCode)),
        "pi" | "pi-agent" => Ok(Some(AgentKind::Pi)),
        _ => Err(format!("不支持的 Agent：{value}")),
    }
}

fn parse_date(value: &str) -> Result<NaiveDate, String> {
    NaiveDate::parse_from_str(value, "%Y-%m-%d")
        .or_else(|_| NaiveDate::parse_from_str(value, "%Y%m%d"))
        .map_err(|_| format!("无效日期：{value}，应为 YYYY-MM-DD 或 YYYYMMDD"))
}

fn local_day_start(date: NaiveDate) -> Result<i64, String> {
    Local
        .from_local_datetime(&date.and_hms_opt(0, 0, 0).unwrap())
        .earliest()
        .map(|value| value.timestamp())
        .ok_or_else(|| format!("无法解析本地日期：{date}"))
}

fn summarize(
    data: &LoadedData,
    agent: Option<AgentKind>,
    since: NaiveDate,
    until: NaiveDate,
) -> BTreeMap<NaiveDate, DailyUsage> {
    let session_models: HashMap<&str, String> = data
        .sessions
        .iter()
        .filter(|session| agent.is_none_or(|agent| session.agent == agent))
        .map(|session| (session.key.as_str(), session.display_model()))
        .collect();
    let mut rows: BTreeMap<NaiveDate, DailyUsage> = BTreeMap::new();
    for turn in data
        .turns
        .iter()
        .filter(|turn| agent.is_none_or(|agent| turn.agent == agent))
    {
        let token_total = turn.input_tokens
            + turn.output_tokens
            + turn.cache_creation_tokens
            + turn.cache_read_tokens;
        if token_total == 0.0 && turn.recorded_cost.unwrap_or(0.0) == 0.0 {
            continue;
        }
        let Some(at) = Local.timestamp_opt(turn.created_at, 0).single() else {
            continue;
        };
        let date = at.date_naive();
        if date < since || date > until {
            continue;
        }
        let model = if turn.model.is_empty() {
            session_models
                .get(turn.session_key.as_str())
                .map(String::as_str)
                .unwrap_or("unknown")
        } else {
            turn.model.as_str()
        };
        let row = rows.entry(date).or_default();
        row.models.insert(model.to_owned());
        row.input += turn.input_tokens;
        row.output += turn.output_tokens;
        row.cache_creation += turn.cache_creation_tokens;
        row.cache_read += turn.cache_read_tokens;
        if let Some(cost) = agg::cost_for_turn(turn, model) {
            row.cost += cost;
        } else {
            row.unpriced_models.insert(model.to_owned());
        }
    }
    rows
}

fn print_table(rows: &BTreeMap<NaiveDate, DailyUsage>) {
    let mut body = Vec::new();
    let mut total = DailyUsage::default();
    for (date, row) in rows {
        total.merge(row);
        body.push(vec![
            date.to_string(),
            row.models.iter().cloned().collect::<Vec<_>>().join(", "),
            format_integer(row.input),
            format_integer(row.output),
            format_integer(row.cache_creation),
            format_integer(row.cache_read),
            format_integer(row.total_tokens()),
            format!("${:.2}", row.cost),
        ]);
    }
    body.push(vec![
        "Total".into(),
        String::new(),
        format_integer(total.input),
        format_integer(total.output),
        format_integer(total.cache_creation),
        format_integer(total.cache_read),
        format_integer(total.total_tokens()),
        format!("${:.2}", total.cost),
    ]);
    let headers = [
        "Date",
        "Models",
        "Input",
        "Output",
        "Cache Create",
        "Cache Read",
        "Total Tokens",
        "Cost (USD)",
    ];
    let widths: Vec<usize> = headers
        .iter()
        .enumerate()
        .map(|(index, header)| {
            body.iter()
                .map(|row| row[index].chars().count())
                .max()
                .unwrap_or(0)
                .max(header.len())
        })
        .collect();
    let separator = widths
        .iter()
        .map(|width| "-".repeat(*width + 2))
        .collect::<Vec<_>>()
        .join("+");
    println!("{separator}");
    print_row(&headers.map(str::to_owned), &widths);
    println!("{separator}");
    for row in body {
        print_row(&row, &widths);
    }
    println!("{separator}");
}

fn print_by_agent_table(
    rows: &BTreeMap<NaiveDate, DailyUsage>,
    agent_rows: &[(AgentKind, BTreeMap<NaiveDate, DailyUsage>)],
) {
    let headers = [
        "Date",
        "Agent",
        "Models",
        "Input",
        "Output",
        "Cache Create",
        "Cache Read",
        "Total Tokens",
        "Cost (USD)",
    ];
    let mut body = Vec::new();
    let mut total = DailyUsage::default();
    for (date, row) in rows {
        total.merge(row);
        body.push(usage_cells(date.to_string(), "All", row));
        for (agent, per_agent) in agent_rows {
            if let Some(agent_row) = per_agent.get(date) {
                body.push(usage_cells(String::new(), agent.label(), agent_row));
            }
        }
    }
    body.push(usage_cells("Total".into(), "All", &total));
    print_grid(&headers, &body, 3);
}

fn usage_cells(date: String, agent: &str, row: &DailyUsage) -> Vec<String> {
    vec![
        date,
        agent.into(),
        row.models.iter().cloned().collect::<Vec<_>>().join(", "),
        format_integer(row.input),
        format_integer(row.output),
        format_integer(row.cache_creation),
        format_integer(row.cache_read),
        format_integer(row.total_tokens()),
        format!("${:.2}", row.cost),
    ]
}

fn print_grid(headers: &[&str], body: &[Vec<String>], numeric_start: usize) {
    let widths: Vec<usize> = headers
        .iter()
        .enumerate()
        .map(|(index, header)| {
            body.iter()
                .map(|row| row[index].chars().count())
                .max()
                .unwrap_or(0)
                .max(header.len())
        })
        .collect();
    let separator = widths
        .iter()
        .map(|width| "-".repeat(*width + 2))
        .collect::<Vec<_>>()
        .join("+");
    println!("{separator}");
    print_row_aligned(
        &headers.iter().map(|s| (*s).to_owned()).collect::<Vec<_>>(),
        &widths,
        numeric_start,
    );
    println!("{separator}");
    for row in body {
        print_row_aligned(row, &widths, numeric_start);
    }
    println!("{separator}");
}

fn print_row<T: AsRef<str>>(row: &[T], widths: &[usize]) {
    print_row_aligned(row, widths, 2);
}

fn print_row_aligned<T: AsRef<str>>(row: &[T], widths: &[usize], numeric_start: usize) {
    let cells = row
        .iter()
        .enumerate()
        .map(|(index, value)| {
            if index >= numeric_start {
                format!(" {:>width$} ", value.as_ref(), width = widths[index])
            } else {
                format!(" {:<width$} ", value.as_ref(), width = widths[index])
            }
        })
        .collect::<Vec<_>>();
    println!("{}", cells.join("|"));
}

fn print_by_agent_csv(
    rows: &BTreeMap<NaiveDate, DailyUsage>,
    agent_rows: &[(AgentKind, BTreeMap<NaiveDate, DailyUsage>)],
) {
    println!("date,agent,models,input,output,cache_creation,cache_read,total_tokens,cost_usd");
    let mut total = DailyUsage::default();
    for (date, row) in rows {
        total.merge(row);
        print_agent_csv_row(&date.to_string(), "all", row);
        for (agent, per_agent) in agent_rows {
            if let Some(agent_row) = per_agent.get(date) {
                print_agent_csv_row("", agent_cli_name(*agent), agent_row);
            }
        }
    }
    print_agent_csv_row("Total", "all", &total);
}

fn agent_cli_name(agent: AgentKind) -> &'static str {
    match agent {
        AgentKind::Devin => "devin",
        AgentKind::Amp => "amp",
        AgentKind::Claude => "claude",
        AgentKind::Codex => "codex",
        AgentKind::Antigravity => "antigravity",
        AgentKind::Grok => "grok",
        AgentKind::ZCode => "zcode",
        AgentKind::OpenCode => "opencode",
        AgentKind::Pi => "pi",
    }
}

fn print_agent_csv_row(date: &str, agent: &str, row: &DailyUsage) {
    println!(
        "{date},\"{}\",\"{}\",{:.0},{:.0},{:.0},{:.0},{:.0},{:.8}",
        agent.replace('"', "\"\""),
        row.models
            .iter()
            .map(|model| model.replace('"', "\"\""))
            .collect::<Vec<_>>()
            .join(", "),
        row.input,
        row.output,
        row.cache_creation,
        row.cache_read,
        row.total_tokens(),
        row.cost,
    );
}

fn print_csv(rows: &BTreeMap<NaiveDate, DailyUsage>) {
    println!("date,models,input,output,cache_creation,cache_read,total_tokens,cost_usd");
    let mut total = DailyUsage::default();
    for (date, row) in rows {
        total.merge(row);
        println!(
            "{date},\"{}\",{:.0},{:.0},{:.0},{:.0},{:.0},{:.8}",
            row.models
                .iter()
                .map(|model| model.replace('"', "\"\""))
                .collect::<Vec<_>>()
                .join(", "),
            row.input,
            row.output,
            row.cache_creation,
            row.cache_read,
            row.total_tokens(),
            row.cost,
        );
    }
    println!(
        "Total,\"\",{:.0},{:.0},{:.0},{:.0},{:.0},{:.8}",
        total.input,
        total.output,
        total.cache_creation,
        total.cache_read,
        total.total_tokens(),
        total.cost,
    );
}

fn format_integer(value: f64) -> String {
    let digits = format!("{:.0}", value);
    let mut formatted = String::with_capacity(digits.len() + digits.len() / 3);
    for (index, ch) in digits.chars().enumerate() {
        if index > 0 && (digits.len() - index) % 3 == 0 {
            formatted.push(',');
        }
        formatted.push(ch);
    }
    formatted
}

fn print_help() {
    println!(
        "Agent Usage Metrics CLI\n\n\
用法：\n  devin-usage-metrics --cli [选项]\n\n\
选项：\n  --agent <name>       Agent 或 all，默认 claude\n  --days <n>           最近 N 天，默认 30\n  --since <date>       起始日期（YYYY-MM-DD 或 YYYYMMDD）\n  --until <date>       结束日期，包含当天\n  --format table|csv   输出格式，默认 table\n  --by-agent           增加按 Agent 分项\n  --refresh            忽略缓存，重新读取本地数据\n  -h, --help           显示帮助\n\n\
示例：\n  devin-usage-metrics --cli --agent claude --since 2026-08-20 --until 2026-08-30 --refresh\n  devin-usage-metrics --cli --agent all --by-agent --since 2026-08-20 --until 2026-08-30\n  devin-usage-metrics --cli --agent claude --days 30 --format csv"
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_cli_date_and_agent_aliases() {
        assert_eq!(parse_date("2026-08-30").unwrap().to_string(), "2026-08-30");
        assert_eq!(parse_date("20260830").unwrap().to_string(), "2026-08-30");
        assert_eq!(parse_agent("claude-code").unwrap(), Some(AgentKind::Claude));
        assert_eq!(parse_agent("open_code").unwrap(), Some(AgentKind::OpenCode));
        assert_eq!(parse_agent("pi-agent").unwrap(), Some(AgentKind::Pi));
        assert_eq!(parse_agent("all").unwrap(), None);
    }

    #[test]
    fn comma_formats_token_counts() {
        assert_eq!(format_integer(349_521_544.0), "349,521,544");
    }

    #[test]
    fn summary_ignores_zero_usage_synthetic_turns() {
        let date = NaiveDate::from_ymd_opt(2026, 8, 24).unwrap();
        let at = local_day_start(date).unwrap() + 3600;
        let data = LoadedData {
            turns: vec![
                crate::data::TurnRec {
                    agent: AgentKind::Claude,
                    created_at: at,
                    model: "<synthetic>".into(),
                    ..Default::default()
                },
                crate::data::TurnRec {
                    agent: AgentKind::Claude,
                    created_at: at,
                    model: "claude-opus-5".into(),
                    output_tokens: 10.0,
                    ..Default::default()
                },
            ],
            ..Default::default()
        };

        let rows = summarize(&data, Some(AgentKind::Claude), date, date);
        assert_eq!(rows[&date].models.len(), 1);
        assert!(rows[&date].models.contains("claude-opus-5"));
    }
}
