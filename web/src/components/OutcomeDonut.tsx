import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from 'recharts'

export interface DonutSlice {
  key: string
  name: string
  value: number
  fill: string
}

/**
 * Donut of run outcomes. Loaded lazily so recharts stays out of the initial
 * bundle — the dashboard is the only page that charts anything.
 */
export default function OutcomeDonut({ slices }: { slices: DonutSlice[] }) {
  const visible = slices.filter((slice) => slice.value > 0)
  return (
    <ResponsiveContainer width="100%" height="100%">
      <PieChart>
        <Pie
          data={visible}
          dataKey="value"
          nameKey="name"
          innerRadius="66%"
          outerRadius="100%"
          paddingAngle={2}
          stroke="none"
          isAnimationActive={false}
        >
          {visible.map((slice) => (
            <Cell key={slice.key} fill={slice.fill} />
          ))}
        </Pie>
        {/* Theme variables, not literals: the tooltip has to be readable on a
            white page as well as a near-black one. */}
        <Tooltip
          contentStyle={{
            background: 'var(--color-slate-900)',
            border: '1px solid var(--color-slate-800)',
            borderRadius: 10,
            fontSize: 12,
            color: 'var(--color-slate-200)',
          }}
          itemStyle={{ color: 'var(--color-slate-200)' }}
        />
      </PieChart>
    </ResponsiveContainer>
  )
}
