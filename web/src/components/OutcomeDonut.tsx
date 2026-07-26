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
        <Tooltip
          contentStyle={{
            background: '#0f172a',
            border: '1px solid #1e293b',
            borderRadius: 10,
            fontSize: 12,
            color: '#e2e8f0',
          }}
          itemStyle={{ color: '#e2e8f0' }}
        />
      </PieChart>
    </ResponsiveContainer>
  )
}
